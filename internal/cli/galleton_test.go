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
