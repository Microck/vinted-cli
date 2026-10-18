// Package throttle keeps the CLI from looking like a bot to the protection in
// front of Vinted (Datadome, Cloudflare), which blocks the user's session when
// requests come too fast or keep coming after a challenge. Every process and
// profile on the machine shares one small JSON file in the XDG state dir,
// changed only under a file lock. Per site (www.vinted.fr, ...) it holds:
//
//   - the cooldown after a block: until it ends no request is sent at all;
//   - the earliest moment the next request may go out, so requests from
//     parallel processes queue behind one another instead of bursting;
//   - the CSRF token and the guest cookie jar, so a command does not load the
//     home page just to get them again.
//
// A Gate also counts one command's requests and stops it at MaxRequests.
package throttle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/filelock"
	"github.com/Microck/vinted-cli/internal/vinted"
)

const (
	// DefaultInterval is the gap between two requests to one site unless
	// config.toml's request_interval says otherwise.
	DefaultInterval = 2 * time.Second
	// MinInterval is the floor for request_interval.
	MinInterval = time.Second
	// MaxJitter bounds the random extra added to every gap, so the requests
	// do not tick like a metronome.
	MaxJitter = time.Second
	// Cooldown is how long every process stays away from a site after a block,
	// unless the server's Retry-After asks for longer.
	Cooldown = 6 * time.Hour
	// MaxRequests is the most requests one command (one watch tick, one MCP
	// tool call) may send.
	MaxRequests = 60
	// csrfTTL bounds how long a cached CSRF token is trusted; Vinted gives it
	// no expiry, and a stale one only costs one extra page load.
	csrfTTL = time.Hour
	// lockWait bounds how long a process waits for another one's lock.
	lockWait = 30 * time.Second
	// maxQueue is the furthest a reserved slot can plausibly be ahead of now;
	// anything later is a clock jump or a hand-edited file and is ignored.
	maxQueue = 10 * time.Minute
	// FileName is the state file inside the state dir.
	FileName = "throttle.json"
)

// Clock is the time source; tests swap in a fake so pacing needs no real sleep.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// System is the real clock.
var System Clock = systemClock{}

// Options tune a Gate. Zero values mean the defaults.
type Options struct {
	// Interval is the gap between requests; below MinInterval it is raised.
	Interval time.Duration
	Clock    Clock
	// Jitter returns the random extra per gap, in [0, MaxJitter).
	Jitter func() time.Duration
	// MaxRequests overrides the per-command budget.
	MaxRequests int
	// Err, when set, is returned by every Wait: a broken request_interval in
	// config.toml stops the commands that would talk to Vinted, but not the
	// ones that fix the config.
	Err error
}

// Gate applies the shared state to one client. It implements vinted.Gate and
// vinted.TokenCache.
type Gate struct {
	path     string
	base     string
	host     string
	interval time.Duration
	clock    Clock
	jitter   func() time.Duration
	max      int
	err      error

	mu   sync.Mutex
	used int
}

var (
	_ vinted.Gate       = (*Gate)(nil)
	_ vinted.TokenCache = (*Gate)(nil)
)

// New returns the gate for the site at base, keeping its state in dir.
func New(dir, base string, o Options) *Gate {
	g := &Gate{path: filepath.Join(dir, FileName), base: base, host: hostOf(base), interval: o.Interval, clock: o.Clock, jitter: o.Jitter, max: o.MaxRequests, err: o.Err}
	if g.interval == 0 {
		g.interval = DefaultInterval
	}
	g.interval = max(g.interval, MinInterval)
	if g.clock == nil {
		g.clock = System
	}
	if g.jitter == nil {
		g.jitter = func() time.Duration { return rand.N(MaxJitter) }
	}
	if g.max == 0 {
		g.max = MaxRequests
	}
	return g
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

// Path is the shared state file.
func (g *Gate) Path() string { return g.path }

// Interval is the effective gap between requests.
func (g *Gate) Interval() time.Duration { return g.interval }

// state is the file. Sites are keyed by host.
type state struct {
	Sites map[string]*site `json:"sites"`
}

type site struct {
	BlockedAt    time.Time        `json:"blocked_at,omitzero"`
	BlockedUntil time.Time        `json:"blocked_until,omitzero"`
	BlockCause   string           `json:"block_cause,omitempty"`
	LastRequest  time.Time        `json:"last_request,omitzero"`
	CSRF         map[string]token `json:"csrf,omitempty"`
	Guest        *guest           `json:"guest,omitempty"`
}

type token struct {
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

type guest struct {
	Cookies map[string]string `json:"cookies"`
	Expires time.Time         `json:"expires"`
}

// load reads the file; a missing one is an empty state. A corrupt one is an
// error rather than a fresh start: silently dropping it would also drop an
// active cooldown.
func (g *Gate) load() (*state, error) {
	st := &state{Sites: map[string]*site{}}
	raw, err := os.ReadFile(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("%s is corrupt (%v). Run `vinted cooldown clear` to reset it", g.path, err)
	}
	if st.Sites == nil {
		st.Sites = map[string]*site{}
	}
	return st, nil
}

// site returns this gate's entry, read without the lock. The file is only
// ever replaced by rename, so a reader sees a whole old or new version.
func (g *Gate) site() (site, error) {
	st, err := g.load()
	if err != nil {
		return site{}, err
	}
	if s := st.Sites[g.host]; s != nil {
		return *s, nil
	}
	return site{}, nil
}

// update runs fn on this site's entry under the lock and saves when fn
// reports a change. reset starts from an empty state when the file is
// unreadable (only `cooldown clear` asks for that).
func (g *Gate) update(ctx context.Context, reset bool, fn func(s *site) bool) error {
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	lock, err := filelock.Acquire(ctx, g.path+".lock")
	if err != nil {
		return fmt.Errorf("waiting for %s.lock: %w", g.path, err)
	}
	defer filelock.Release(lock)
	st, err := g.load()
	if err != nil {
		if !reset {
			return err
		}
		st = &state{Sites: map[string]*site{}}
	}
	s := st.Sites[g.host]
	if s == nil {
		s = &site{}
		st.Sites[g.host] = s
	}
	if !fn(s) {
		return nil
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the guest jar holds tokens.
	return config.WriteAtomic(g.path, append(raw, '\n'), 0o600)
}

// Wait implements vinted.Gate: it refuses during a cooldown or past the
// budget, otherwise reserves the next free slot for this site and sleeps
// until it comes. Slots are reserved under the lock, so parallel processes
// line up instead of all firing at once.
func (g *Gate) Wait(ctx context.Context) error {
	if g.err != nil {
		return g.err
	}
	g.mu.Lock()
	g.used++
	used := g.used
	g.mu.Unlock()
	if used > g.max {
		return &vinted.Error{Kind: vinted.KindUsage, Msg: fmt.Sprintf("this command reached the limit of %d requests to vinted and stopped, so the session does not get flagged as automated. Ask for less at once (fewer --pages, fewer watches per check)", g.max)}
	}
	var slot time.Time
	var refused error
	err := g.update(ctx, false, func(s *site) bool {
		now := g.clock.Now()
		if now.Before(s.BlockedUntil) {
			refused = g.cooldownError(*s, now)
			return false
		}
		// The gap is measured from the last reserved slot with this
		// process's interval, so a changed request_interval applies at once.
		slot = now
		last := s.LastRequest
		// A slot far beyond any queue means a clock jump or a hand-edited
		// file; it must not stall every command.
		if last.After(now.Add(maxQueue)) {
			last = time.Time{}
		}
		if next := last.Add(g.interval + g.jitter()); !last.IsZero() && next.After(slot) {
			slot = next
		}
		s.LastRequest = slot
		return true
	})
	if err != nil {
		return err
	}
	if refused != nil {
		return refused
	}
	if err := g.clock.Sleep(ctx, slot.Sub(g.clock.Now())); err != nil {
		return err
	}
	// Another process may have been blocked while this one waited.
	return g.Check()
}

// Block implements vinted.Gate: it starts (or keeps, if longer) the site's
// cooldown and forgets the cached guest jar and CSRF tokens, whose cookies
// the protection has just flagged.
func (g *Gate) Block(cause string, retryAfter time.Duration) (time.Duration, error) {
	now := g.clock.Now()
	until := now.Add(max(Cooldown, retryAfter))
	err := g.update(context.Background(), false, func(s *site) bool {
		if s.BlockedUntil.After(until) {
			until = s.BlockedUntil
		} else {
			s.BlockedAt, s.BlockedUntil, s.BlockCause = now, until, cause
		}
		s.Guest, s.CSRF = nil, nil
		return true
	})
	return until.Sub(now), err
}

func (g *Gate) cooldownError(s site, now time.Time) error {
	left := s.BlockedUntil.Sub(now)
	msg := fmt.Sprintf("vinted blocked requests from this machine at %s, so the cli will not contact %s until %s (%s left): every request before then extends the block. Stop running vinted commands, open %s in your normal browser, and if it shows a captcha or a block page solve it there; if the block persists, re-export your cookies with `vinted auth login`",
		stamp(s.BlockedAt), g.host, stamp(s.BlockedUntil), vinted.FormatWait(left), g.base)
	return &vinted.Error{Kind: vinted.KindBlocked, Msg: msg, Suggest: vinted.BlockAdvice(g.base, left)}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "an unknown time"
	}
	return t.Local().Format("2006-01-02 15:04 MST")
}

// Reset starts a fresh request budget: each tick of a long-running loop
// (`watch run`, `chat open`) counts as its own command.
func (g *Gate) Reset() {
	g.mu.Lock()
	g.used = 0
	g.mu.Unlock()
}

// Exhausted reports that the budget is spent: the next Wait would refuse.
func (g *Gate) Exhausted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used >= g.max
}

// Check returns the cooldown error while the site is cooling down and nil
// otherwise. It sends nothing and takes no slot or budget.
func (g *Gate) Check() error {
	s, err := g.site()
	if err != nil {
		return err
	}
	if now := g.clock.Now(); now.Before(s.BlockedUntil) {
		return g.cooldownError(s, now)
	}
	return nil
}

// Status is the site's cooldown as `vinted cooldown`, `doctor` and
// `auth status` show it.
type Status struct {
	Site            string     `json:"site"`
	Blocked         bool       `json:"blocked"`
	BlockedAt       *time.Time `json:"blocked_at,omitempty"`
	BlockedUntil    *time.Time `json:"blocked_until,omitempty"`
	Remaining       string     `json:"remaining,omitempty"`
	Cause           string     `json:"cause,omitempty"`
	RequestInterval string     `json:"request_interval"`
	File            string     `json:"file"`
}

// Status reads the site's cooldown without touching the network.
func (g *Gate) Status() (Status, error) {
	s, err := g.site()
	if err != nil {
		return Status{Site: g.host, RequestInterval: g.interval.String(), File: g.path}, err
	}
	return g.status(s), nil
}

func (g *Gate) status(s site) Status {
	st := Status{Site: g.host, RequestInterval: g.interval.String(), File: g.path}
	now := g.clock.Now()
	if now.Before(s.BlockedUntil) {
		at, until := s.BlockedAt, s.BlockedUntil
		st.Blocked, st.BlockedAt, st.BlockedUntil = true, &at, &until
		st.Remaining, st.Cause = vinted.FormatWait(until.Sub(now)), s.BlockCause
	}
	return st
}

// Clear lifts the site's cooldown, the only way to end one early, and returns
// what it was. The cached guest jar and CSRF tokens go too, so the next
// request starts from a clean slate. An unreadable file is reset.
func (g *Gate) Clear(ctx context.Context) (Status, error) {
	var before site
	err := g.update(ctx, true, func(s *site) bool {
		before = *s
		s.BlockedAt, s.BlockedUntil, s.BlockCause = time.Time{}, time.Time{}, ""
		s.Guest, s.CSRF = nil, nil
		return true
	})
	return g.status(before), err
}

// CSRF implements vinted.TokenCache.
func (g *Gate) CSRF(user int64) string {
	s, err := g.site()
	if err != nil {
		return ""
	}
	t, ok := s.CSRF[strconv.FormatInt(user, 10)]
	if !ok || !g.clock.Now().Before(t.Expires) {
		return ""
	}
	return t.Value
}

// SaveCSRF implements vinted.TokenCache.
func (g *Gate) SaveCSRF(user int64, tok string) error {
	exp := g.clock.Now().Add(csrfTTL)
	return g.update(context.Background(), false, func(s *site) bool {
		if s.CSRF == nil {
			s.CSRF = map[string]token{}
		}
		s.CSRF[strconv.FormatInt(user, 10)] = token{Value: tok, Expires: exp}
		return true
	})
}

// DropCSRF implements vinted.TokenCache.
func (g *Gate) DropCSRF(user int64) error {
	key := strconv.FormatInt(user, 10)
	return g.update(context.Background(), false, func(s *site) bool {
		if _, ok := s.CSRF[key]; !ok {
			return false
		}
		delete(s.CSRF, key)
		return true
	})
}

// Guest implements vinted.TokenCache: the cached guest jar while its access
// token has more than a minute left, else nil.
func (g *Gate) Guest() map[string]string {
	s, err := g.site()
	if err != nil || s.Guest == nil || time.Until(s.Guest.Expires) < time.Minute {
		return nil
	}
	return s.Guest.Cookies
}

// SaveGuest implements vinted.TokenCache.
func (g *Gate) SaveGuest(cookies map[string]string, expires time.Time) error {
	return g.update(context.Background(), false, func(s *site) bool {
		s.Guest = &guest{Cookies: cookies, Expires: expires}
		return true
	})
}
