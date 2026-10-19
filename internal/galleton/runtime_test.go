package galleton

import (
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/filelock"
	"github.com/Microck/vinted-cli/internal/galleton/bundle"
)

func TestMain(m *testing.M) {
	if handled, err := HandleHost(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// testSetup is a minimal valid adapter: the lifecycle tests never renew.
var testSetup = Setup{Adapter: []byte(`{"providers":[{"name":"vinted-test","kind":"http","origins":["https://www.vinted.test"],"refresh_url":"https://www.vinted.test/web/api/auth/refresh","refresh_method":"POST","body_format":"json","refresh_interval_seconds":3600}]}`)}

func bundledRuntime(t *testing.T) string {
	t.Helper()
	if len(bundle.Binary) == 0 {
		t.Skip("run with -tags galleton_bundle after go run ./cmd/bundle-galleton")
	}
	for _, key := range []string{"VINTED_GALLETON_DIR", "VINTED_GALLETON_URL", "VINTED_GALLETON_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	dir := filepath.Join(t.TempDir(), "sessions")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = Stop(ctx, dir)
	})
	return dir
}
func TestBundledConcurrentStartupAndLeaseLifetime(t *testing.T) {
	dir := bundledRuntime(t)
	const n = 8
	var wg sync.WaitGroup
	clients := make([]*Client, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) { defer wg.Done(); clients[i], errs[i] = Open(context.Background(), dir, testSetup) }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()
	raw, err := readRegular(filepath.Join(dir, "endpoint.json"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	var ep endpoint
	if json.Unmarshal(raw, &ep) != nil || ep.Version != bundle.Version {
		t.Fatal("unexpected endpoint")
	}
	for _, c := range clients {
		if _, err := c.Status(context.Background(), "missing"); !IsStatus(err, 404) {
			t.Fatalf("real SDK/daemon contract: %v", err)
		}
	}
	if err := Stop(context.Background(), dir); err == nil {
		t.Fatal("stopped an engine with active clients")
	}
	for i := 0; i < n-1; i++ {
		clients[i].Close()
	}
	if _, err := running(context.Background(), dir, ""); err != nil {
		t.Fatal("remaining client lost its daemon:", err)
	}
	for _, file := range []string{"api.token", "endpoint.json", "adapters.json"} {
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode %o", file, info.Mode().Perm())
		}
	}
	clients[n-1].Close()
	if err := Stop(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
}

// A daemon starts with every provider paused; Launched runs once, for the
// Open that started it, while that hold is on, and its resume ends it.
func TestBundledStartHoldsProvidersUntilLaunched(t *testing.T) {
	dir := bundledRuntime(t)
	var calls int
	setup := testSetup
	setup.Launched = func(c *Client) error {
		calls++
		providers, err := c.sdk.Providers(context.Background())
		if err != nil {
			return err
		}
		if len(providers) != 1 || providers[0].PausedUntil == nil || time.Until(*providers[0].PausedUntil) > launchHold {
			return fmt.Errorf("providers at launch: %+v", providers)
		}
		return c.Resume(context.Background(), "vinted-test")
	}
	c, err := Open(context.Background(), dir, setup)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	again, err := Open(context.Background(), dir, setup)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	if calls != 1 {
		t.Fatalf("Launched ran %d times, want once for the start", calls)
	}
	providers, err := c.sdk.Providers(context.Background())
	if err != nil || len(providers) != 1 || providers[0].PausedUntil != nil {
		t.Fatalf("providers after Launched: %+v %v", providers, err)
	}
}

func TestHoldArgsPauseEveryProvider(t *testing.T) {
	args, err := holdArgs([]byte(`{"providers":[{"name":"vinted"},{"name":"vinted-fr"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); got != "--pause vinted=60 --pause vinted-fr=60" {
		t.Fatalf("hold args %q", got)
	}
	if _, err := holdArgs([]byte(`not json`)); err == nil {
		t.Fatal("accepted an invalid adapter")
	}
}
func TestBundledIdleShutdownAndRestart(t *testing.T) {
	dir := bundledRuntime(t)
	c, err := Open(context.Background(), dir, testSetup)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := os.ReadFile(filepath.Join(dir, "api.token"))
	// A crashed command may leave a lease file, but its OS lock is released.
	if err := os.WriteFile(filepath.Join(dir, "leases", "crashed.lease"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	c.Close()
	deadline := time.Now().Add(idleGrace + 6*time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "endpoint.json")); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "endpoint.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("idle daemon did not shut down")
	}
	c, err = Open(context.Background(), dir, testSetup)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	next, _ := os.ReadFile(filepath.Join(dir, "api.token"))
	if string(token) != string(next) {
		t.Fatal("restart regenerated the vault API token")
	}
}
func TestBundledRepairAndCanceledStartup(t *testing.T) {
	dir := bundledRuntime(t)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	path, err := engineBinary(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupted binary"), 0700); err != nil {
		t.Fatal(err)
	}
	repaired, err := engineBinary(context.Background(), dir)
	if err != nil || repaired != path {
		t.Fatal("repair failed:", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(bundle.Binary) {
		t.Fatal("corrupt cache was executed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, dir, testSetup); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "api.token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled open started daemon")
	}
}
func TestStateSymlinkAndSetupMismatchFailClosed(t *testing.T) {
	dir := bundledRuntime(t)
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(t.TempDir(), link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(context.Background(), link, testSetup); err == nil {
			t.Fatal("accepted symlink state directory")
		}
	}
	c, err := Open(context.Background(), dir, testSetup)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Another invocation with a different egress must not borrow this daemon.
	other := Setup{Adapter: testSetup.Adapter, Proxy: "socks5h://127.0.0.1:9"}
	if c2, err := Open(context.Background(), dir, other); !errors.Is(err, errHostConfigMismatch) {
		if c2 != nil {
			c2.Close()
		}
		t.Fatalf("attached to a daemon with a different proxy: %v", err)
	}
}
func TestLeasesArePrunedOnlyAfterRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.lease")
	lease, err := filelock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	active, err := activeLeases(dir)
	if err != nil || !active {
		t.Fatal("lost a live lease", err)
	}
	filelock.Release(lease)
	active, err = activeLeases(dir)
	if err != nil || active {
		t.Fatal("stale lease remained live", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale lease not removed")
	}
}
func TestHostDoesNotInheritProviderCredentials(t *testing.T) {
	t.Setenv("VINTED_PROXY", "socks5h://user:secret@127.0.0.1:1080")
	t.Setenv("VINTED_GALLETON_TOKEN_FILE", "/secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	for _, v := range cleanEnv() {
		if strings.HasPrefix(v, "VINTED_") || v == "GITHUB_TOKEN=github-secret" {
			t.Fatal("secret inherited")
		}
	}
}

func TestSourceBuildPreparesPinnedEngine(t *testing.T) {
	if len(bundle.Binary) != 0 || os.Getenv("VINTED_TEST_SOURCE_BUILD") != "1" {
		t.Skip("opt-in test of the plain go-install preparation path")
	}
	dir := filepath.Join(t.TempDir(), "source-engine")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	path, err := engineBinary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil || info.Main.Path != bundle.Module {
		t.Fatalf("source provenance: %+v %v", info, err)
	}
	// The verified cache must work without Go on PATH on subsequent invocations.
	t.Setenv("PATH", t.TempDir())
	cached, err := engineBinary(ctx, dir)
	if err != nil || cached != path {
		t.Fatalf("cache reuse: %s %v", cached, err)
	}
}

// A host that is draining releases startup.lock before host.lock. An Open
// that slips into that window must launch the replacement once host.lock
// frees up, not wait out its deadline against a missing endpoint.
func TestBundledOpenLaunchesAfterADrainingHostExits(t *testing.T) {
	dir := bundledRuntime(t)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	draining, err := filelock.Acquire(context.Background(), filepath.Join(dir, "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		filelock.Release(draining)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Open(ctx, dir, testSetup)
	if err != nil {
		t.Fatal("did not take over after the draining host exited:", err)
	}
	c.Close()
}
