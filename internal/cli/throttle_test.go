package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func datadomeBlock(t *testing.T) *fakeBlock {
	return &fakeBlock{status: http.StatusForbidden, body: fixture(t, "datadome_403.html")}
}

// throttleState reads the shared cooldown and pacing file.
func (h *harness) throttleState() map[string]map[string]any {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.home, "state", "vinted-cli", "throttle.json"))
	if err != nil {
		h.t.Fatalf("throttle state: %v", err)
	}
	var st struct {
		Sites map[string]map[string]any `json:"sites"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	return st.Sites
}

func (h *harness) site() string {
	return strings.TrimPrefix(h.fake.srv.URL, "http://")
}

func TestBlockStartsACooldownThatSendsNothing(t *testing.T) {
	h := newHarness(t)
	h.fake.setBlock("/", datadomeBlock(t))

	r := h.run("", "search", "nike")
	if r.code != 5 {
		t.Fatalf("blocked search: exit %d\n%s", r.code, r.stderr)
	}
	for _, want := range []string{"stop running vinted commands", "normal browser", "vinted auth login", "6h"} {
		if !strings.Contains(strings.ToLower(r.stderr), want) {
			t.Errorf("block message lacks %q:\n%s", want, r.stderr)
		}
	}
	// One request, refused, and nothing after it: no retry of any kind.
	if n := h.fake.requestCount(); n != 1 {
		t.Fatalf("a blocked command sent %d requests", n)
	}

	site := h.throttleState()[h.site()]
	until, err := time.Parse(time.RFC3339Nano, site["blocked_until"].(string))
	if err != nil {
		t.Fatalf("blocked_until: %v", site)
	}
	if d := until.Sub(h.clock.Now()); d < 6*time.Hour-time.Minute || d > 6*time.Hour+time.Minute {
		t.Fatalf("cooldown lasts %v, want 6h", d)
	}

	// Even with Vinted answering again, every command fails fast, offline.
	h.fake.setBlock("", nil)
	before := h.fake.requestCount()
	h.clock.advance(time.Hour)
	for _, args := range [][]string{{"search", "nike"}, {"item", "show", fixtureItemRef()}, {"doctor"}} {
		r := h.run("", args...)
		if r.code != 5 || !strings.Contains(r.stderr, "5h0m left") {
			t.Fatalf("%v during the cooldown: exit %d\n%s", args, r.code, r.stderr)
		}
	}
	if n := h.fake.requestCount(); n != before {
		t.Fatalf("%d requests reached vinted during the cooldown", n-before)
	}

	cd := decode[map[string]any](t, h.must("", "cooldown").stdout)
	if cd["blocked"] != true || cd["remaining"] != "5h0m" || cd["site"] != h.site() {
		t.Fatalf("cooldown status %v", cd)
	}
	st := decode[map[string]any](t, h.must("", "auth", "status").stdout)
	if c, _ := st["cooldown"].(map[string]any); c["blocked"] != true {
		t.Fatalf("auth status does not show the cooldown: %v", st)
	}

	// Lifting it is explicit: scripts must say --yes.
	if r := h.run("", "cooldown", "clear"); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("clear without --yes: exit %d\n%s", r.code, r.stderr)
	}
	cleared := decode[map[string]any](t, h.must("", "cooldown", "clear", "--yes").stdout)
	if cleared["cleared"] != true {
		t.Fatalf("clear: %v", cleared)
	}
	h.must("", "search", "nike")
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != false {
		t.Fatalf("still blocked after clear: %v", cd)
	}
}

func TestRetryAfterLongerThanTheCooldownIsHonoured(t *testing.T) {
	h := newHarness(t)
	h.fake.setBlock("/", &fakeBlock{status: http.StatusTooManyRequests, body: []byte("slow down"), retryAfter: "36000"})
	if r := h.run("", "search", "nike"); r.code != 5 {
		t.Fatalf("rate limited: exit %d\n%s", r.code, r.stderr)
	}
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["remaining"] != "10h0m" {
		t.Fatalf("cooldown %v, want the 10h Retry-After", cd)
	}
}

func TestBlockMidCommandIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.login()
	// The account and home page work; the catalog is blocked.
	h.fake.setBlock("/catalog", datadomeBlock(t))
	if r := h.run("", "search", "nike", "--pages", "3"); r.code != 5 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	if n := len(h.fake.requestsTo("/catalog")); n != 1 {
		t.Fatalf("the blocked page was requested %d times", n)
	}
}

func TestWatchCheckStopsAtTheFirstBlock(t *testing.T) {
	h := newHarness(t)
	h.must("", "watch", "add", "search", "nike", "--name", "one")
	h.must("", "watch", "add", "search", "adidas", "--name", "two")
	h.fake.setBlock("/catalog", datadomeBlock(t))
	before := len(h.fake.requestsTo("/catalog"))
	r := h.run("", "watch", "check", "--all", "--format", "jsonl")
	if r.code != 5 {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
	if n := len(h.fake.requestsTo("/catalog")) - before; n != 1 {
		t.Fatalf("checks after the block sent %d catalog requests", n)
	}
	before = h.fake.requestCount()
	if r := h.run("", "watch", "check", "--all"); r.code != 5 || h.fake.requestCount() != before {
		t.Fatalf("watch check during the cooldown: exit %d, %d requests", r.code, h.fake.requestCount()-before)
	}
}

func TestRequestsArePacedAcrossCommands(t *testing.T) {
	h := newHarness(t)
	h.must("", "search", "nike")
	h.must("", "search", "nike", "--page", "2")
	sent := h.clock.sent()
	if len(sent) < 3 {
		t.Fatalf("only %d gated requests", len(sent))
	}
	for i := 1; i < len(sent); i++ {
		gap := sent[i].Sub(sent[i-1])
		if gap < 2*time.Second || gap >= 3*time.Second {
			t.Fatalf("request %d went out %v after the previous one; want 2s plus under 1s of jitter", i, gap)
		}
	}
	// The second command reused the cached guest session: no second home page load.
	if n := len(h.fake.requestsTo("/")); n != 1 {
		t.Fatalf("home page loaded %d times", n)
	}

	if r := h.run("", "config", "set", "request_interval", "500ms"); r.code != 2 {
		t.Fatalf("interval under the floor accepted: exit %d", r.code)
	}
	h.must("", "config", "set", "request_interval", "5s")
	h.must("", "search", "nike")
	sent = h.clock.sent()
	if gap := sent[len(sent)-1].Sub(sent[len(sent)-2]); gap < 5*time.Second {
		t.Fatalf("request_interval 5s, gap %v", gap)
	}
}

func TestPageAndIntervalCaps(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"search", "nike", "--pages", "6"},
		{"watch", "add", "search", "nike", "--name", "w", "--pages", "6"},
		{"watch", "add", "search", "nike", "--name", "w", "--interval", "10m"},
		{"watch", "add", "seller", "123456789", "--name", "w", "--interval", "5m"},
		{"watch", "run", "--interval", "14m"},
		{"watch", "service", "install", "--interval", "5m"},
		{"chat", "open", "1", "--interval", "5s"},
	} {
		if r := h.run("", args...); r.code != 2 {
			t.Fatalf("%v: exit %d, want 2\n%s", args, r.code, r.stderr)
		}
	}
	if n := h.fake.requestCount(); n != 0 {
		t.Fatalf("rejected commands sent %d requests", n)
	}
	h.must("", "search", "nike", "--pages", "5")
	if n := len(h.fake.requestsTo("/catalog")); n != 5 {
		t.Fatalf("--pages 5 fetched %d pages", n)
	}
	// A watch defaults to the 15m floor.
	w := decode[map[string]any](t, h.must("", "watch", "add", "search", "nike", "--name", "w").stdout)
	if iv, _ := w["interval"].(float64); time.Duration(iv) != 15*time.Minute {
		t.Fatalf("default watch interval %v", w["interval"])
	}
}
