package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/galleton"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/throttle"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/spf13/cobra"
)

// serviceTick is how often `auth service run` checks the engine; serviceStop
// ends it (tests only: nil never fires).
var (
	serviceTick = 10 * time.Second
	serviceStop <-chan struct{}
)

func (a *App) authServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage optional unattended session renewal",
		Long: `The bundled Galleton session engine renews the stored session while vinted
commands, watches or the MCP server run, and stops ten seconds after the
last one exits. "enable" keeps it running between commands and restarts it
at OS login (a systemd user unit, a LaunchAgent, or a Windows logon task),
so the session renews even when nothing else runs. The unit carries the
paths and VINTED_PROXY of the shell that enabled it, never a credential.`,
	}
	var dir string
	run := &cobra.Command{Use: "run", Short: "Keep session renewal running until interrupted", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		state := a.sessionDir()
		if dir != "" {
			state = dir
		}
		// Attaches, and holds the renewals for any cooldown recorded while no
		// engine ran. During a cooldown the engine is never started, so wait
		// it out here: a Windows logon task would not run again until the
		// next logon.
		var c *galleton.Client
		for {
			var err error
			c, err = a.openEngine(cmd.Context(), state)
			var held *engineHeldError
			if err == nil {
				break
			}
			if !errors.As(err, &held) {
				return err
			}
			select {
			case <-cmd.Context().Done():
				return nil
			case <-serviceStop:
				return nil
			case <-time.After(serviceTick):
			}
		}
		defer c.Close()
		reported := map[string]time.Time{}
		tick := time.NewTicker(serviceTick)
		defer tick.Stop()
		for {
			select {
			case <-cmd.Context().Done():
				return nil
			case <-serviceStop:
				return nil
			case <-tick.C:
				probe, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
				err := c.Ping(probe)
				cancel()
				if err != nil {
					// Returning an error lets an explicitly installed OS service restart.
					return err
				}
				if err := a.recordEngineBlocks(cmd.Context(), c, reported); err != nil {
					return err
				}
				// The engine caps a pause at 24 h while a cooldown can last
				// longer (Retry-After); keep extending it while one runs.
				if err := a.carryCooldown(cmd.Context(), c); err != nil {
					return err
				}
			}
		}
	}}
	run.Flags().StringVar(&dir, "dir", "", "absolute private state directory (used by installed services)")
	cmd.AddCommand(run,
		&cobra.Command{Use: "enable", Short: "Enable unattended renewal at OS login", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.authServiceChange(cmd.Context(), true) }},
		&cobra.Command{Use: "disable", Short: "Remove unattended renewal at OS login", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.authServiceChange(cmd.Context(), false) }},
		&cobra.Command{Use: "status", Short: "Inspect the session engine without starting it", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			state := galleton.RuntimeStatus(cmd.Context(), a.sessionDir())
			unit, _, err := a.authServiceUnit()
			if err == nil {
				state["startup_installed"] = unit.installed()
			}
			return a.Printer.Print(state)
		}},
		&cobra.Command{Use: "stop", Short: "Stop an idle session engine without deleting credentials", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if galleton.Configured() {
				return output.Usagef("external daemon lifecycle is managed by its owner")
			}
			if _, err := os.Stat(a.sessionDir()); errors.Is(err, os.ErrNotExist) {
				return a.Printer.Print(map[string]bool{"stopped": true})
			}
			if err := galleton.Stop(cmd.Context(), a.sessionDir()); err != nil {
				return err
			}
			return a.Printer.Print(map[string]bool{"stopped": true})
		}},
	)
	return cmd
}

// recordEngineBlocks starts the shared cooldown for a site whose scheduled
// renewal the bot protection blocked. Nothing else reports it: the engine
// only backs off that session, and no command saw the block. reported holds
// the retry time already handled per session, so one block counts once.
func (a *App) recordEngineBlocks(ctx context.Context, c *galleton.Client, reported map[string]time.Time) error {
	sessions, err := c.List(ctx)
	if err != nil {
		return err
	}
	active, err := a.Gate.Cooldowns()
	if err != nil {
		return err
	}
	for _, s := range sessions {
		// A retry time already past is a back-off the engine is about to end
		// with a new attempt, not a fresh block.
		if s.Status != "retry_wait" || s.Error == nil || s.Error.Code != "upstream_blocked" || !time.Now().Before(s.NextRefresh) || reported[s.ID].Equal(s.NextRefresh) {
			continue
		}
		reported[s.ID] = s.NextRefresh
		base := vinted.ProviderBase(s.Provider, a.Client.Base)
		if base == "" {
			continue
		}
		u, err := url.Parse(base)
		if err != nil {
			continue
		}
		if until, ok := active[u.Host]; ok && !until.Before(s.NextRefresh) {
			continue // a running cooldown already covers the engine's retry
		}
		g := throttle.New(a.Paths.StateDir, base, throttle.Options{Clock: throttleClock})
		wait, err := g.Block("vinted's bot protection (Datadome/Cloudflare) blocked the session engine's scheduled token renewal", time.Until(s.NextRefresh))
		if err != nil {
			return err
		}
		// Hold the provider too, for every caller: only `auth service run`
		// re-applies cooldowns as pauses on its own.
		if err := c.Pause(ctx, s.Provider, wait); err != nil {
			return fmt.Errorf("could not pause the session engine for the cooldown on %s: %w", u.Host, managedError(err))
		}
	}
	return nil
}

func (a *App) authServiceUnit() (*serviceUnit, string, error) {
	dir, err := filepath.Abs(a.sessionDir())
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(dir))
	name := fmt.Sprintf("vinted-session-%x", sum[:8])
	switch runtime.GOOS {
	case "linux":
		return &serviceUnit{platform: "systemd", name: name + ".service", files: []string{filepath.Join(config.UserHome(), ".config", "systemd", "user", name+".service")}}, dir, nil
	case "darwin":
		return &serviceUnit{platform: "launchd", name: "dev.micr." + name, files: []string{filepath.Join(config.UserHome(), "Library", "LaunchAgents", "dev.micr."+name+".plist")}}, dir, nil
	case "windows":
		return &serviceUnit{platform: "schtasks", name: name, files: []string{filepath.Join(dir, "startup-installed")}}, dir, nil
	default:
		return nil, "", output.Usagef("startup installation is not supported on %s; use `vinted auth service run` with your process supervisor", runtime.GOOS)
	}
}

// Service definitions use absolute paths and the same environment freeze as
// the watch timer (paths, VINTED_BASE_URL, VINTED_PROXY), so the service
// starts the engine with the egress the enabling shell used. They never copy
// a credential: enable refuses a VINTED_PROXY carrying one.
func authServiceDefinition(platform, name, exe, dir string, env [][2]string) string {
	if platform == "systemd" {
		return "[Unit]\nDescription=Vinted session renewal\n\n[Service]\nType=simple\n" + systemdEnv(env) + "ExecStart=" + systemdQuote(exe) + " auth service run --dir " + systemdQuote(dir) + " --no-input\nRestart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n"
	}
	var envDict strings.Builder
	if len(env) > 0 {
		envDict.WriteString("<key>EnvironmentVariables</key><dict>")
		for _, kv := range env {
			envDict.WriteString("<key>" + xmlEscape(kv[0]) + "</key><string>" + xmlEscape(kv[1]) + "</string>")
		}
		envDict.WriteString("</dict>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + xmlEscape(name) + `</string>
<key>ProgramArguments</key><array><string>` + xmlEscape(exe) + `</string><string>auth</string><string>service</string><string>run</string><string>--dir</string><string>` + xmlEscape(dir) + `</string><string>--no-input</string></array>
` + envDict.String() + `<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
</dict></plist>
`
}

func (a *App) authServiceChange(ctx context.Context, enable bool) error {
	if galleton.Configured() {
		return output.Usagef("clear VINTED_GALLETON_* overrides to manage the bundled engine's startup")
	}
	if enable {
		if err := checkServiceProxies(); err != nil {
			return err
		}
	}
	u, dir, err := a.authServiceUnit()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	// Keep the installed path, not a Homebrew version's resolved target: upgrades
	// replace that symlink. Daemon bootstrap resolves it before executing.
	for _, p := range []string{dir, exe, u.files[0]} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n") {
			return output.Usagef("startup requires absolute single-line paths")
		}
	}
	command := func(name string, args ...string) error {
		if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
			return fmt.Errorf("%s failed: %w; startup configuration was retained for retry", name, err)
		}
		return nil
	}
	if enable {
		if u.platform == "schtasks" {
			if err := checkLogonEnv(serviceEnv()); err != nil {
				return err
			}
		}
		// Prove startup works before registering it with the user's OS session.
		if _, err := a.galletonClient(ctx); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(u.files[0]), 0700); err != nil {
			return err
		}
		if u.platform != "schtasks" {
			env := serviceEnv()
			if u.platform == "launchd" {
				env = launchdEnv(env)
			}
			if err := os.WriteFile(u.files[0], []byte(authServiceDefinition(u.platform, u.name, exe, dir, env)), 0600); err != nil {
				return err
			}
		}
		switch u.platform {
		case "systemd":
			if err := command("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			err = command("systemctl", "--user", "enable", "--now", u.name)
		case "launchd":
			_ = exec.CommandContext(ctx, "launchctl", "unload", u.files[0]).Run()
			err = command("launchctl", "load", u.files[0])
		case "schtasks":
			task := fmt.Sprintf(`"%s" auth service run --dir "%s" --no-input`, exe, dir)
			if err = command("schtasks", "/Create", "/SC", "ONLOGON", "/TN", u.name, "/TR", task, "/IT", "/F"); err == nil {
				// Keep the marker even if launching fails, so disable can unregister.
				err = os.WriteFile(u.files[0], []byte(u.name), 0600)
				if err == nil {
					err = command("schtasks", "/Run", "/TN", u.name)
				}
			}
		}
	} else if u.installed() {
		switch u.platform {
		case "systemd":
			err = command("systemctl", "--user", "disable", "--now", u.name)
		case "launchd":
			err = command("launchctl", "unload", u.files[0])
		case "schtasks":
			_ = exec.CommandContext(ctx, "schtasks", "/End", "/TN", u.name).Run()
			err = command("schtasks", "/Delete", "/TN", u.name, "/F")
		}
		if err == nil {
			err = os.Remove(u.files[0])
			if u.platform == "systemd" && err == nil {
				err = command("systemctl", "--user", "daemon-reload")
			}
		}
	}
	if err != nil {
		return err
	}
	return a.Printer.Print(map[string]any{"startup_enabled": enable, "platform": u.platform, "unit": u.name, "state_dir": dir})
}
