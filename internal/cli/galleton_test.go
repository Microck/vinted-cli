package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/Microck/galleton/client"

	"github.com/Microck/vinted-cli/internal/testutil"
)

// engineClient connects to the session engine a command left running (it
// stays up for a short idle grace after the last command).
func (h *harness) engineClient() *sdk.Client {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.engineDir(), "endpoint.json"))
	if err != nil {
		h.t.Fatalf("the session engine is not running: %v", err)
	}
	var ep struct{ URL string }
	if err := json.Unmarshal(raw, &ep); err != nil {
		h.t.Fatal(err)
	}
	c, err := sdk.FromDir(ep.URL, h.engineDir())
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) pausedUntil() *time.Time {
	h.t.Helper()
	providers, err := h.engineClient().Providers(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range providers {
		if p.Name == "vinted-custom" {
			return p.PausedUntil
		}
	}
	h.t.Fatal("the engine has no provider for the fake site")
	return nil
}

// A block seen on an account request pauses the session engine too, so it
// renews nothing while the site blocks; clearing the cooldown lifts both.
func TestBlockPausesTheEngineUntilCooldownClear(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/api/v2/users/current", datadomeBlock(t))
	if r := h.run("", "me", "show"); r.code != 5 {
		t.Fatalf("blocked me show: exit %d\n%s", r.code, r.stderr)
	}
	until := h.pausedUntil()
	if until == nil || time.Until(*until) < 5*time.Hour {
		t.Fatalf("engine pause %v, want about 6h", until)
	}
	h.fake.setBlock("", nil)
	h.must("", "cooldown", "clear", "--yes")
	if until := h.pausedUntil(); until != nil {
		t.Fatalf("engine still paused until %v after cooldown clear", until)
	}
	h.must("", "me", "show")
}

// One engine, one egress: a command with a different proxy never borrows a
// running engine that would renew through another route.
func TestEngineRefusesADifferentProxy(t *testing.T) {
	h := newHarness(t)
	h.login()
	t.Setenv("VINTED_PROXY", "socks5h://"+testutil.ClosedAddr(t))
	r := h.run("", "me", "show")
	if r.code == 0 || !strings.Contains(r.stderr, "different proxy") {
		t.Fatalf("attached to an engine with another egress: exit %d\n%s", r.code, r.stderr)
	}
}

func TestAuthServiceStatusAndStop(t *testing.T) {
	h := newHarness(t)
	st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout)
	if st["running"] != false || st["external"] != false {
		t.Fatalf("status before any login %v", st)
	}
	h.login()
	if st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout); st["running"] != true {
		t.Fatalf("status after login %v", st)
	}
	if out := decode[map[string]any](t, h.must("", "auth", "service", "stop").stdout); out["stopped"] != true {
		t.Fatalf("stop %v", out)
	}
	if st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout); st["running"] != false {
		t.Fatalf("status after stop %v", st)
	}
	// The vault outlives the engine: the next command restarts it.
	h.must("", "me", "show")
}

// A new login is imported beside the current session and replaces it only
// once it works: a rejected one leaves the profile on its old session, and a
// good one removes the old session from the engine.
func TestReloginReplacesTheSessionOnlyWhenTheNewOneWorks(t *testing.T) {
	h := newHarness(t)
	first := decode[map[string]any](t, h.login().stdout)["session_id"].(string)

	bad := filepath.Join(t.TempDir(), "spent.txt")
	if err := os.WriteFile(bad, []byte("access_token_web=x; refresh_token_web=spent-refresh-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "auth", "login", "--cookies", bad); r.code != 3 {
		t.Fatalf("login with a spent token: exit %d\n%s", r.code, r.stderr)
	}
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["session_id"] != first {
		t.Fatalf("a rejected login replaced the session: %v", st)
	}
	h.must("", "me", "show")

	second := decode[map[string]any](t, h.login().stdout)["session_id"].(string)
	if second == first {
		t.Fatal("a new login reused the old session id")
	}
	sessions, err := h.engineClient().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != second {
		t.Fatalf("engine sessions after relogin: %+v", sessions)
	}
	h.must("", "me", "show")
}

// A block on the engine's own renewal starts the shared cooldown and the
// engine pause, the same as a block the CLI saw on one of its requests.
func TestBlockedEngineRenewalStartsTheCooldown(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	r := h.run("", "auth", "refresh")
	if r.code != 5 || !strings.Contains(r.stderr, "renewal") {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("no cooldown after a blocked renewal: %v", cd)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("the engine was not paused after its renewal was blocked")
	}
}

// A block seen by a logged-out profile still pauses the running engine,
// which renews another profile's session for the same site.
func TestGuestBlockPausesARunningEngine(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/catalog", datadomeBlock(t))
	if r := h.run("", "search", "nike", "--profile", "guest"); r.code != 5 {
		t.Fatalf("blocked guest search: exit %d\n%s", r.code, r.stderr)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("a guest block left the running engine renewing")
	}
}
