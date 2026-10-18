package galleton

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/filelock"
	"github.com/Microck/vinted-cli/internal/galleton/bundle"
)

const hostCommand = "__galleton-host"
const idleGrace = 10 * time.Second

// ProxyEnv carries the egress proxy from the CLI to the host and from the host
// to the daemon. The adapter names it with proxy_env, so a proxy URL with
// credentials never lands in a file or on a command line.
const ProxyEnv = "VINTED_GALLETON_PROXY"

var errHostVersionMismatch = errors.New("a different session engine version is still running; run `vinted auth service disable && vinted auth service enable`, or stop watch/MCP processes, then retry")

var errHostConfigMismatch = errors.New("the running session engine uses a different proxy or Vinted site configuration. Run the command with the same VINTED_PROXY/config proxy, or stop the engine first (`vinted auth service stop`; disable the auth service if it is enabled). No credentials were discarded")

// Setup is what a daemon is started with: the adapter configuration and the
// egress proxy every upstream call (renewals included) must use. Clients only
// attach to a daemon started with the same Setup, so one invocation can never
// send a renewal out through another invocation's egress.
type Setup struct {
	Adapter []byte
	Proxy   string
}

func (s Setup) fingerprint() string {
	h := sha256.New()
	h.Write(s.Adapter)
	h.Write([]byte{0})
	h.Write([]byte(s.Proxy))
	return hex.EncodeToString(h.Sum(nil))
}

type endpoint struct {
	URL     string `json:"url"`
	Version string `json:"version"`
	Config  string `json:"config"`
}

// Open obtains a lease on the shared, automatically managed daemon. Releasing
// the last lease allows the host to shut down after a short idle grace period.
// Only read-only readiness probes are retried; imports/renewals are never replayed.
func Open(ctx context.Context, dir string, setup Setup) (*Client, error) {
	if Configured() {
		return FromEnv()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	lock, err := filelock.Acquire(ctx, filepath.Join(dir, "startup.lock"))
	if err != nil {
		return nil, err
	}
	defer filelock.Release(lock)
	leases := filepath.Join(dir, "leases")
	if err := privateDir(leases); err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	leasePath := filepath.Join(leases, hex.EncodeToString(id[:])+".lease")
	lease, err := filelock.Acquire(ctx, leasePath)
	if err != nil {
		return nil, err
	}
	release := func() { filelock.Release(lease); os.Remove(leasePath) }
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	connect := func() (*Client, error) {
		c, err := running(ctx, dir, setup.fingerprint())
		if err == nil {
			c.release = release
			success = true
		}
		return c, err
	}
	c, connectErr := connect()
	if connectErr == nil {
		return c, nil
	}
	host, free, err := filelock.Try(filepath.Join(dir, "host.lock"))
	if err != nil {
		return nil, err
	}
	if !free && (errors.Is(connectErr, errHostVersionMismatch) || errors.Is(connectErr, errHostConfigMismatch)) {
		return nil, connectErr
	}
	launch := func() error {
		// Prepare before launching so source-build errors are returned to the user.
		if _, err := engineBinary(ctx, dir); err != nil {
			return err
		}
		// No host is running, so nothing reads the adapter now. Proxy, pacing
		// and size changes keep imported sessions; a changed site definition
		// makes the daemon refuse that site's sessions until reimported.
		if err := atomicFile(filepath.Join(dir, "adapters.json"), setup.Adapter, 0600); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		exe, err = filepath.EvalSymlinks(exe)
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, hostCommand, dir)
		cmd.Env = withProxy(cleanEnv(), setup.Proxy)
		detach(cmd)
		// Never inherit terminal pipes: the short-lived CLI must be free to exit.
		if err := cmd.Start(); err != nil {
			return errors.New("could not start the bundled session host")
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	if free {
		filelock.Release(host)
		if err := launch(); err != nil {
			return nil, err
		}
	}
	// Generous: a loaded machine can take a while to extract and start the
	// engine, and a failure here is only a slower error message.
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		c, err := connect()
		if err == nil {
			return c, nil
		}
		if !free {
			// The host seen at the first check may have been draining: it
			// removes its endpoint and releases startup.lock before host.lock.
			// Once host.lock frees up, launch the replacement ourselves. While
			// it is held, a host that was still starting may have only just
			// published its endpoint, so a version or setup conflict is final.
			host, unlocked, lockErr := filelock.Try(filepath.Join(dir, "host.lock"))
			if lockErr != nil {
				return nil, lockErr
			}
			if !unlocked && (errors.Is(err, errHostVersionMismatch) || errors.Is(err, errHostConfigMismatch)) {
				return nil, err
			}
			if unlocked {
				filelock.Release(host)
				free = true
				if err := launch(); err != nil {
					return nil, err
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("session host did not become ready; run `vinted auth service status` (no credentials were discarded)")
		case <-tick.C:
		}
	}
}

// withProxy adds the egress proxy to a cleaned environment.
func withProxy(env []string, proxy string) []string {
	if proxy == "" {
		return env
	}
	return append(env, ProxyEnv+"="+proxy)
}

// running connects to the published daemon. want is the Setup fingerprint the
// caller needs; empty accepts any (status and stop).
func running(ctx context.Context, dir, want string) (*Client, error) {
	raw, err := readRegular(filepath.Join(dir, "endpoint.json"), 4096)
	if err != nil {
		return nil, err
	}
	var ep endpoint
	if json.Unmarshal(raw, &ep) != nil {
		return nil, errors.New("invalid session host endpoint")
	}
	if ep.Version != bundle.Version {
		return nil, errHostVersionMismatch
	}
	if want != "" && ep.Config != want {
		return nil, errHostConfigMismatch
	}
	token, err := readRegular(filepath.Join(dir, "api.token"), 4096)
	if err != nil {
		return nil, err
	}
	c, err := New(ep.URL, string(token))
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, ep.URL+"/v1/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, errors.New("session host is not responding")
	}
	defer resp.Body.Close()
	var health struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&health) != nil || !health.OK || health.Version != bundle.EngineVersion {
		return nil, errors.New("session host failed authenticated readiness check")
	}
	return c, nil
}

// HandleHost is called before argument parsing in the executable. It is not a
// public CLI command and never installs a service or handles user credentials.
func HandleHost(args []string) (bool, error) {
	if len(args) == 0 || args[0] != hostCommand {
		return false, nil
	}
	if len(args) != 2 {
		return true, errors.New("invalid session host invocation")
	}
	return true, RunHost(args[1])
}

// RunHost owns one daemon per state directory. A vault lock inside Galleton is
// a second safeguard against duplicate credential rotation across processes.
func RunHost(dir string) error {
	if err := privateDir(dir); err != nil {
		return err
	}
	host, ok, err := filelock.Try(filepath.Join(dir, "host.lock"))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer filelock.Release(host)
	ctx, stop := signal.NotifyContext(context.Background(), hostSignals()...)
	defer stop()
	binary, err := engineBinary(ctx, dir)
	if err != nil {
		return err
	}
	adapter, err := readRegular(filepath.Join(dir, "adapters.json"), 1<<20)
	if err != nil {
		return err
	}
	setup := Setup{Adapter: adapter, Proxy: os.Getenv(ProxyEnv)}
	init := exec.CommandContext(ctx, binary, "init", "--dir", dir)
	init.Env = cleanEnv()
	noWindow(init)
	if err := init.Run(); err != nil {
		return errors.New("could not initialize private session storage")
	}
	cmd := exec.Command(binary, "serve", "--dir", dir, "--config", filepath.Join(dir, "adapters.json"), "--listen", "127.0.0.1:0")
	cmd.Env = withProxy(cleanEnv(), setup.Proxy)
	noWindow(cmd)
	pipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return errors.New("could not start the session engine")
	}
	done := make(chan error, 1)
	ready := make(chan string, 1)
	// The engine reports its actual bound address. Never reserve-and-release a
	// port or send the API token to a guessed listener.
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "Galleton listening on ") {
				fields := strings.Fields(line)
				if len(fields) > 3 {
					host, port, err := net.SplitHostPort(fields[3])
					p, _ := strconv.Atoi(port)
					if err == nil && host == "127.0.0.1" && p > 0 && p <= 65535 {
						select {
						case ready <- "http://" + fields[3]:
						default:
						}
					}
				}
			}
		}
	}()
	go func() { done <- cmd.Wait() }()
	var c *Client
	var shutdownGuard *os.File
	defer func() {
		// Do not interrupt a pending rotation immediately. The daemon drains its
		// scheduler and checkpoints on authenticated shutdown.
		if c != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_ = c.Shutdown(shutdown)
			cancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Minute):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.Remove(filepath.Join(dir, "endpoint.json"))
		filelock.Release(shutdownGuard)
	}()
	select {
	case base := <-ready:
		token, err := readRegular(filepath.Join(dir, "api.token"), 4096)
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		c, err = New(base, string(token))
		if err != nil {
			_ = cmd.Process.Kill()
			return err
		}
		raw, _ := json.Marshal(endpoint{URL: base, Version: bundle.Version, Config: setup.fingerprint()})
		if err := atomicFile(filepath.Join(dir, "endpoint.json"), raw, 0600); err != nil {
			return err
		}
		// Also validates the token, URL and health response before accepting leases.
		_, err = running(ctx, dir, setup.fingerprint())
		if err != nil {
			return err
		}
	case err := <-done:
		// Leave the completion available to the deferred drain.
		done <- err
		return errors.New("session engine stopped before becoming ready")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return ctx.Err()
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		return errors.New("session engine startup timed out")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastActive := time.Now()
	for {
		select {
		case err := <-done:
			done <- err
			return errors.New("session engine stopped unexpectedly; retrying a command will inspect its saved checkpoints")
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// Serializes the last-client check with new clients acquiring their leases.
			lock, ok, err := filelock.Try(filepath.Join(dir, "startup.lock"))
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			active, err := activeLeases(filepath.Join(dir, "leases"))
			if active {
				lastActive = time.Now()
			}
			if err == nil && !active && time.Since(lastActive) >= idleGrace {
				// Hold this lock until shutdown has drained. New clients cannot connect
				// to an instance halfway through shutdown.
				shutdownGuard = lock
				return nil
			}
			filelock.Release(lock)
			if err != nil {
				return err
			}
		}
	}
}

func activeLeases(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	active := false
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".lease") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, free, err := filelock.Try(path)
		if err != nil {
			return false, err
		}
		if !free {
			active = true
			continue
		}
		filelock.Release(f)
		_ = os.Remove(path)
	}
	return active, nil
}

// RuntimeStatus is metadata only: checking it does not start a daemon.
func RuntimeStatus(ctx context.Context, dir string) map[string]any {
	if Configured() {
		c, err := FromEnv()
		if err == nil {
			probe, cancel := context.WithTimeout(ctx, time.Second)
			err = c.Ping(probe)
			cancel()
		}
		return map[string]any{"running": err == nil, "external": true}
	}
	_, err := running(ctx, dir, "")
	return map[string]any{"running": err == nil, "state_dir": dir, "engine_version": bundle.Version, "external": false}
}

func Stop(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	lock, err := filelock.Acquire(ctx, filepath.Join(dir, "startup.lock"))
	if err != nil {
		return err
	}
	defer filelock.Release(lock)
	active, err := activeLeases(filepath.Join(dir, "leases"))
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("session engine is in use; stop the auth service, watch, or MCP process first")
	}
	c, err := running(ctx, dir, "")
	if err != nil {
		return nil
	}
	if err := c.Shutdown(ctx); err != nil {
		return err
	}
	// Keep startup serialized until the old host has fully drained and released
	// its vault. A successful shutdown acknowledgement alone is not completion.
	host, err := filelock.Acquire(ctx, filepath.Join(dir, "host.lock"))
	if err != nil {
		return err
	}
	defer filelock.Release(host)
	vault, err := filelock.Acquire(ctx, filepath.Join(dir, "daemon.lock"))
	if err == nil {
		filelock.Release(vault)
	}
	return err
}
