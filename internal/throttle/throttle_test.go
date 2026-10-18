package throttle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/vinted"
)

// fakeClock advances only when slept on and records when each sleep ended,
// which is when the gated request would have gone out.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	releases []time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
	}
	c.releases = append(c.releases, c.now)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

const base = "https://www.vinted.fr"

func newGate(t *testing.T, dir string, clock *fakeClock, o Options) *Gate {
	t.Helper()
	o.Clock = clock
	if o.Jitter == nil {
		o.Jitter = func() time.Duration { return 300 * time.Millisecond }
	}
	return New(dir, base, o)
}

func kindOf(err error) vinted.Kind {
	var ve *vinted.Error
	if errors.As(err, &ve) {
		return ve.Kind
	}
	return -1
}

func TestPacingIsSharedBetweenProcesses(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	// Two gates on one dir stand for two processes.
	a, b := newGate(t, dir, clock, Options{}), newGate(t, dir, clock, Options{})
	ctx := context.Background()
	for _, g := range []*Gate{a, b, a} {
		if err := g.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	r := clock.releases
	if len(r) != 3 {
		t.Fatalf("releases %v", r)
	}
	for i := 1; i < len(r); i++ {
		if gap := r[i].Sub(r[i-1]); gap < DefaultInterval {
			t.Fatalf("request %d went out %v after the previous one, below %v", i, gap, DefaultInterval)
		}
		if gap := r[i].Sub(r[i-1]); gap != DefaultInterval+300*time.Millisecond {
			t.Fatalf("gap %v, want interval plus jitter", gap)
		}
	}
	// Once the gap has passed on its own, nothing waits.
	clock.advance(time.Minute)
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if last := clock.releases[3]; !last.Equal(clock.Now()) {
		t.Fatal("an idle site should not make the request wait")
	}
}

func TestIntervalFloorAndDefaultJitter(t *testing.T) {
	g := New(t.TempDir(), base, Options{Interval: 100 * time.Millisecond})
	if g.Interval() != MinInterval {
		t.Fatalf("interval %v, want the %v floor", g.Interval(), MinInterval)
	}
	for range 100 {
		if j := g.jitter(); j < 0 || j >= MaxJitter {
			t.Fatalf("jitter %v out of [0, %v)", j, MaxJitter)
		}
	}
}

func TestBlockStartsASharedCooldown(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	g := newGate(t, dir, clock, Options{})
	wait, err := g.Block("HTTP 403 on GET /catalog", 0)
	if err != nil || wait != Cooldown {
		t.Fatalf("wait %v err %v", wait, err)
	}
	other := newGate(t, dir, clock, Options{})
	clock.advance(time.Hour)
	err = other.Wait(context.Background())
	if kindOf(err) != vinted.KindBlocked || !strings.Contains(err.Error(), "5h0m left") || !strings.Contains(err.Error(), "normal browser") || !strings.Contains(err.Error(), "vinted auth login") {
		t.Fatalf("err = %v", err)
	}
	if len(clock.releases) != 0 {
		t.Fatal("a refused request must not wait for a slot")
	}
	st, err := other.Status()
	if err != nil || !st.Blocked || st.Remaining != "5h0m" || st.Cause != "HTTP 403 on GET /catalog" {
		t.Fatalf("status %+v err %v", st, err)
	}
	// Another site is not affected.
	if err := New(dir, "https://www.vinted.es", Options{Clock: clock}).Wait(context.Background()); err != nil {
		t.Fatalf("other site: %v", err)
	}
	before, err := other.Clear(context.Background())
	if err != nil || !before.Blocked {
		t.Fatalf("clear: %+v %v", before, err)
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("after clear: %v", err)
	}
	if st, _ := g.Status(); st.Blocked {
		t.Fatal("still blocked after clear")
	}
}

func TestCooldownEndsOnItsOwn(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	g := newGate(t, t.TempDir(), clock, Options{})
	if _, err := g.Block("x", 0); err != nil {
		t.Fatal(err)
	}
	clock.advance(Cooldown)
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("after the cooldown: %v", err)
	}
}

func TestRetryAfterOnlyLengthensTheCooldown(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	g := newGate(t, t.TempDir(), clock, Options{})
	if wait, _ := g.Block("x", time.Minute); wait != Cooldown {
		t.Fatalf("short Retry-After shortened the cooldown to %v", wait)
	}
	if wait, _ := g.Block("x", 10*time.Hour); wait != 10*time.Hour {
		t.Fatalf("long Retry-After ignored: %v", wait)
	}
	// A later, shorter block keeps the longer cooldown.
	if wait, _ := g.Block("x", 0); wait != 10*time.Hour {
		t.Fatalf("a second block shortened the cooldown to %v", wait)
	}
}

func TestBudgetStopsTheCommand(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	g := newGate(t, t.TempDir(), clock, Options{MaxRequests: 3})
	ctx := context.Background()
	for range 3 {
		if err := g.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	err := g.Wait(ctx)
	if kindOf(err) != vinted.KindUsage || !strings.Contains(err.Error(), "limit of 3 requests") {
		t.Fatalf("err = %v", err)
	}
	g.Reset()
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if New(t.TempDir(), base, Options{}).max != MaxRequests {
		t.Fatal("default budget")
	}
}

func TestConfigErrorStopsRequests(t *testing.T) {
	want := errors.New("bad request_interval")
	if err := New(t.TempDir(), base, Options{Err: want}).Wait(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v", err)
	}
}

func TestCorruptStateFailsClosedUntilCleared(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Now()}
	g := newGate(t, dir, clock, Options{})
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "cooldown clear") {
		t.Fatalf("err = %v", err)
	}
	if _, err := g.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFarFutureSlotIsCapped(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	g := newGate(t, dir, clock, Options{})
	if err := g.update(context.Background(), false, func(s *site) bool {
		s.LastRequest = clock.Now().Add(24 * time.Hour)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	start := clock.Now()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := clock.Now().Sub(start); waited > DefaultInterval+MaxJitter {
		t.Fatalf("waited %v for a corrupt slot", waited)
	}
}

func TestTokenCache(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Now()}
	g := newGate(t, dir, clock, Options{})
	if err := g.SaveCSRF(42, "tok-42"); err != nil {
		t.Fatal(err)
	}
	if g.CSRF(42) != "tok-42" || g.CSRF(0) != "" {
		t.Fatal("csrf lookup by account")
	}
	if err := g.SaveGuest(map[string]string{"access_token_web": "g"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if g.Guest()["access_token_web"] != "g" {
		t.Fatal("guest jar not cached")
	}
	fi, err := os.Stat(g.Path())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode %v err %v", fi.Mode().Perm(), err)
	}
	clock.advance(csrfTTL)
	if g.CSRF(42) != "" {
		t.Fatal("expired csrf token served")
	}
	if err := g.SaveCSRF(42, "tok-42"); err != nil {
		t.Fatal(err)
	}
	if err := g.DropCSRF(42); err != nil || g.CSRF(42) != "" {
		t.Fatal("drop")
	}
	if err := g.SaveCSRF(42, "tok-42"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Block("x", 0); err != nil {
		t.Fatal(err)
	}
	if g.CSRF(42) != "" || g.Guest() != nil {
		t.Fatal("a block must forget the flagged tokens")
	}
	if err := g.SaveGuest(map[string]string{"access_token_web": "g"}, time.Now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if g.Guest() != nil {
		t.Fatal("a guest jar about to expire is not worth reusing")
	}
}
