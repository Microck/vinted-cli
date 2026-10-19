package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/cli"
)

const renewalPrompt = "keep this login alive in the background? [Y/n]"

// renewalHarness fakes the terminal and the service manager: enabling
// writes the unit under the temp HOME and records systemctl calls.
func renewalHarness(t *testing.T, tty bool) (*harness, func() []string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the renewal service tests use the systemd layout")
	}
	t.Setenv("CI", "")
	h := newHarness(t)
	var mu sync.Mutex
	var calls []string
	t.Cleanup(cli.UseServiceExec(func(name string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}))
	t.Cleanup(cli.UsePromptTerminal(tty))
	return h, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func (h *harness) renewalUnits() []string {
	units, _ := filepath.Glob(filepath.Join(h.home, ".config", "systemd", "user", "vinted-session-*.service"))
	return units
}

func enabled(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "systemctl --user enable --now vinted-session-") {
			return true
		}
	}
	return false
}

func TestLoginPromptEnablesRenewal(t *testing.T) {
	for _, answer := range []string{"\n", "y\n", "YES\n"} {
		h, calls := renewalHarness(t, true)
		r := h.must(answer, "auth", "login", "--cookies", h.fake.cookieFile(t))
		if !strings.Contains(r.stderr, renewalPrompt) || !strings.Contains(r.stderr, "background renewal on (systemd vinted-session-") {
			t.Fatalf("answer %q: %s", answer, r.stderr)
		}
		if !enabled(calls()) || len(h.renewalUnits()) != 1 {
			t.Fatalf("answer %q: not enabled: %v %v", answer, calls(), h.renewalUnits())
		}
		if st := decode[map[string]any](t, r.stdout); st["logged_in"] != true {
			t.Fatalf("stdout must stay the login status: %s", r.stdout)
		}
	}
}

func TestLoginPromptNoPrintsHint(t *testing.T) {
	for _, answer := range []string{"n\n", "no\n", ""} {
		h, calls := renewalHarness(t, true)
		r := h.must(answer, "auth", "login", "--cookies", h.fake.cookieFile(t))
		if !strings.Contains(r.stderr, renewalPrompt) || !strings.Contains(r.stderr, "run `vinted auth service enable` later to turn it on") {
			t.Fatalf("answer %q: %s", answer, r.stderr)
		}
		if len(calls()) != 0 || len(h.renewalUnits()) != 0 {
			t.Fatalf("answer %q installed the service: %v", answer, calls())
		}
	}
}

func TestLoginDoesNotAskWithoutATerminalOrInCI(t *testing.T) {
	cases := map[string]func(t *testing.T) (*harness, func() []string, []string){
		"no-input": func(t *testing.T) (*harness, func() []string, []string) {
			h, c := renewalHarness(t, true)
			return h, c, []string{"--no-input"}
		},
		"not a terminal": func(t *testing.T) (*harness, func() []string, []string) {
			h, c := renewalHarness(t, false)
			return h, c, nil
		},
		"ci": func(t *testing.T) (*harness, func() []string, []string) {
			h, c := renewalHarness(t, true)
			t.Setenv("CI", "true")
			return h, c, nil
		},
		"no-service": func(t *testing.T) (*harness, func() []string, []string) {
			h, c := renewalHarness(t, true)
			return h, c, []string{"--no-service"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h, calls, extra := setup(t)
			r := h.must("y\n", append([]string{"auth", "login", "--cookies", h.fake.cookieFile(t)}, extra...)...)
			if strings.Contains(r.stderr, renewalPrompt) || len(calls()) != 0 {
				t.Fatalf("asked or installed: %s %v", r.stderr, calls())
			}
		})
	}
}

func TestLoginServiceFlagEnablesWithoutAsking(t *testing.T) {
	h, calls := renewalHarness(t, false)
	r := h.must("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--no-input", "--service")
	if strings.Contains(r.stderr, renewalPrompt) || !enabled(calls()) || len(h.renewalUnits()) != 1 {
		t.Fatalf("%s %v", r.stderr, calls())
	}
	if r := h.run("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--service", "--no-service"); r.code != 2 || !strings.Contains(r.stderr, "exclusive") {
		t.Fatalf("both flags: %d %s", r.code, r.stderr)
	}
}

func TestLoginSkipsThePromptWhenRenewalIsInstalled(t *testing.T) {
	h, calls := renewalHarness(t, true)
	h.must("\n", "auth", "login", "--cookies", h.fake.cookieFile(t))
	before := len(calls())
	r := h.must("\n", "auth", "login", "--cookies", h.fake.cookieFile(t))
	if strings.Contains(r.stderr, renewalPrompt) || len(calls()) != before {
		t.Fatalf("asked again: %s %v", r.stderr, calls())
	}
	// --service always enables again: that retries an enable that failed
	// after writing the unit file.
	r = h.must("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--service")
	if !strings.Contains(r.stderr, "background renewal on") || len(calls()) == before {
		t.Fatalf("--service did not enable again: %s %v", r.stderr, calls())
	}
}

// setRefreshExpiry rewrites the stored refresh token expiry, recorded as
// issued now, after any renewal the engine has made.
func (h *harness) setRefreshExpiry(at time.Time) {
	h.t.Helper()
	h.setRefresh(time.Now(), at)
}

func (h *harness) setRefresh(issued, expires time.Time) {
	h.t.Helper()
	raw, err := os.ReadFile(h.credentialsFile())
	if err != nil {
		h.t.Fatal(err)
	}
	for key, at := range map[string]time.Time{"refresh_expires": expires, "refresh_issued": issued} {
		re := regexp.MustCompile(`(?m)^` + key + ` = .*$`)
		if !re.Match(raw) {
			h.t.Fatalf("no %s in credentials.toml:\n%s", key, raw)
		}
		raw = re.ReplaceAll(raw, []byte(key+" = "+at.UTC().Format(time.RFC3339Nano)))
	}
	if err := os.WriteFile(h.credentialsFile(), raw, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func renewalCheck(t *testing.T, h *harness) map[string]any {
	t.Helper()
	rep := decode[struct {
		OK     bool             `json:"ok"`
		Checks []map[string]any `json:"checks"`
	}](t, h.must("", "doctor").stdout)
	if !rep.OK {
		t.Fatalf("a renewal warning must not fail doctor: %+v", rep)
	}
	for _, c := range rep.Checks {
		if c["name"] == "renewal" {
			return c
		}
	}
	return nil
}

func TestRenewalWarningThresholds(t *testing.T) {
	h, _ := renewalHarness(t, false)
	h.must("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--no-service")
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["refresh_expires"] == nil || st["warning"] != nil {
		t.Fatalf("a fresh 7-day login: %v", st)
	}
	for _, c := range []struct {
		in   time.Duration
		warn bool
	}{{72 * time.Hour, false}, {49 * time.Hour, false}, {47 * time.Hour, true}, {time.Hour, true}, {-time.Hour, true}} {
		h.setRefreshExpiry(time.Now().Add(c.in))
		r := h.must("", "auth", "status")
		st := decode[map[string]any](t, r.stdout)
		if got := st["warning"] != nil; got != c.warn || strings.Contains(r.stderr, "vinted: warning:") != c.warn {
			t.Fatalf("expiry in %s: warning %v, want %v: %v %s", c.in, got, c.warn, st, r.stderr)
		}
		check := renewalCheck(t, h)
		if (check != nil) != c.warn || (check != nil && (check["ok"] != true || check["warning"] != true)) {
			t.Fatalf("doctor, expiry in %s: %v", c.in, check)
		}
	}
	// An installed service keeps the login alive: no warning.
	h.must("", "auth", "service", "enable")
	h.setRefreshExpiry(time.Now().Add(time.Hour))
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["warning"] != nil {
		t.Fatalf("warned with the service installed: %v", st)
	}
	if c := renewalCheck(t, h); c != nil {
		t.Fatalf("doctor warned with the service installed: %v", c)
	}
}

// A renewal by the engine issues a new refresh token: the recorded expiry
// moves forward by the recorded lifetime and the warning clears.
func TestEngineRenewalMovesTheRefreshExpiry(t *testing.T) {
	h, _ := renewalHarness(t, false)
	h.must("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--no-service")
	h.setRefresh(time.Now().Add(-167*time.Hour), time.Now().Add(time.Hour))
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["warning"] == nil {
		t.Fatalf("no warning an hour before expiry: %v", st)
	}
	h.must("", "auth", "refresh")
	st := decode[map[string]any](t, h.must("", "auth", "status").stdout)
	exp, _ := time.Parse(time.RFC3339, st["refresh_expires"].(string))
	if st["warning"] != nil || time.Until(exp) < 160*time.Hour {
		t.Fatalf("renewal did not move the expiry: %v", st)
	}
}

// A failed enable from the login leaves no unit file behind, so the next
// login asks again instead of reading a half-installed service as on.
func TestFailedEnableFromLoginIsNotInstalled(t *testing.T) {
	h, _ := renewalHarness(t, true)
	t.Cleanup(cli.UseServiceExec(func(name string, args ...string) error {
		if name == "systemctl" && len(args) > 1 && args[1] == "enable" {
			return errors.New("exit status 1")
		}
		return nil
	}))
	r := h.must("\n", "auth", "login", "--cookies", h.fake.cookieFile(t))
	if !strings.Contains(r.stderr, "background renewal could not be enabled") || len(h.renewalUnits()) != 0 {
		t.Fatalf("%s %v", r.stderr, h.renewalUnits())
	}
	if r := h.run("", "auth", "login", "--cookies", h.fake.cookieFile(t), "--service"); r.code == 0 || len(h.renewalUnits()) != 0 {
		t.Fatalf("--service: %d %s %v", r.code, r.stderr, h.renewalUnits())
	}
	if r := h.must("n\n", "auth", "login", "--cookies", h.fake.cookieFile(t)); !strings.Contains(r.stderr, renewalPrompt) {
		t.Fatalf("not asked again: %s", r.stderr)
	}
}
