package vinted

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingGate counts what the client asked of the gate.
type recordingGate struct {
	mu         sync.Mutex
	waits      int
	blocks     []string
	retryAfter time.Duration
	refuse     error
}

func (g *recordingGate) Wait(context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse != nil {
		return g.refuse
	}
	g.waits++
	return nil
}

func (g *recordingGate) Block(cause string, retryAfter time.Duration) (time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocks = append(g.blocks, cause)
	g.retryAfter = retryAfter
	return 6 * time.Hour, nil
}

// memTokens is an in-memory TokenCache.
type memTokens struct {
	csrf  map[int64]string
	guest map[string]string
	drops int
}

func (m *memTokens) CSRF(user int64) string { return m.csrf[user] }
func (m *memTokens) SaveCSRF(user int64, tok string) error {
	m.csrf[user] = tok
	return nil
}
func (m *memTokens) DropCSRF(user int64) error {
	delete(m.csrf, user)
	m.drops++
	return nil
}
func (m *memTokens) Guest() map[string]string { return m.guest }
func (m *memTokens) SaveGuest(c map[string]string, _ time.Time) error {
	m.guest = c
	return nil
}

func TestIsBlock(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   []byte
		want   bool
	}{
		{"datadome captcha page", 403, fixture(t, "datadome_403.html"), true},
		{"cloudflare challenge", 403, fixture(t, "cloudflare_403.html"), true},
		{"rate limit", 429, []byte("slow down"), true},
		{"datadome json", 403, []byte(`{"url":"https://geo.captcha-delivery.com/captcha/?initialCid=x"}`), true},
		{"datadome page served as 200", 200, fixture(t, "datadome_403.html"), true},
		{"session blocked notice", 401, []byte(`{"code":0,"message":"Se ha bloqueado tu sesión"}`), true},
		{"unknown 403 page", 403, []byte("<html>forbidden</html>"), true},
		{"vinted permission error", 403, []byte(`{"code":106,"message":"csrf"}`), false},
		{"real catalog page", 200, fixture(t, "catalog.html"), false},
		{"not found", 404, []byte(`{"code":404,"message":"Not found"}`), false},
	}
	for _, tc := range cases {
		if got := isBlock(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: isBlock = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBlockIsFinalAndRecorded(t *testing.T) {
	f, c := newFake(t)
	g := &recordingGate{}
	c.Gate = g
	f.override["GET /catalog"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "36000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(fixture(t, "datadome_403.html"))
	}
	_, err := c.Search(context.Background(), SearchParams{Text: "x"})
	var ve *Error
	if !errors.As(err, &ve) || ve.Kind != KindBlocked || ve.Retryable {
		t.Fatalf("err = %#v", err)
	}
	for _, want := range []string{"stop running vinted commands", "normal browser", "vinted auth login", "6h"} {
		if !strings.Contains(strings.ToLower(ve.Msg), strings.ToLower(want)) {
			t.Errorf("message lacks %q: %s", want, ve.Msg)
		}
	}
	if f.count("GET /catalog") != 1 {
		t.Fatalf("a blocked request was retried: %d hits", f.count("GET /catalog"))
	}
	if len(g.blocks) != 1 || g.retryAfter != 10*time.Hour {
		t.Fatalf("blocks=%v retryAfter=%v", g.blocks, g.retryAfter)
	}
}

// A block on an account request records the cooldown and pauses the
// account in the session engine for as long, so it renews nothing meanwhile.
func TestBlockPausesTheAccount(t *testing.T) {
	f, c := newFake(t)
	g := &recordingGate{}
	c.Gate = g
	acct := loggedIn(c)
	f.override["GET /api/v2/users/current"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><script src="https://ct.captcha-delivery.com/c.js"></script></html>`))
	}
	if _, err := c.Me(context.Background()); kindOf(err) != KindBlocked {
		t.Fatalf("err = %v", err)
	}
	if f.count("GET /api/v2/users/current") != 1 || len(g.blocks) != 1 {
		t.Fatalf("hits %d, blocks %v", f.count("GET /api/v2/users/current"), g.blocks)
	}
	if len(acct.pauses) != 1 || acct.pauses[0] != 6*time.Hour {
		t.Fatalf("account pauses %v, want one of 6h", acct.pauses)
	}
}

func TestGateWaitsBeforeEveryRequestAndCanRefuse(t *testing.T) {
	f, c := newFake(t)
	g := &recordingGate{}
	c.Gate = g
	if _, err := c.Search(context.Background(), SearchParams{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if g.waits != f.count("GET /")+f.count("GET /catalog") {
		t.Fatalf("waits %d for %d requests", g.waits, f.count("GET /")+f.count("GET /catalog"))
	}
	g.refuse = &Error{Kind: KindBlocked, Msg: "cooling down"}
	before := f.count("GET /catalog")
	if _, err := c.Search(context.Background(), SearchParams{Text: "x"}); kindOf(err) != KindBlocked {
		t.Fatalf("err = %v", err)
	}
	if f.count("GET /catalog") != before {
		t.Fatal("a refused request reached the server")
	}
}

func TestCachedGuestJarSkipsHomePage(t *testing.T) {
	f, c := newFake(t)
	tokens := &memTokens{csrf: map[int64]string{}}
	c.Tokens = tokens
	if _, err := c.Search(context.Background(), SearchParams{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /") != 1 || tokens.guest[AccessCookie] == "" {
		t.Fatalf("home hits %d, cached guest %v", f.count("GET /"), tokens.guest != nil)
	}
	// The next command (a new client) reuses the cached guest jar.
	next := New()
	next.Base, next.Tokens = c.Base, tokens
	if _, err := next.Search(context.Background(), SearchParams{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /") != 1 {
		t.Fatalf("the cached guest jar was not reused: %d home hits", f.count("GET /"))
	}
}

func TestCachedCSRFIsReusedAndReplacedWhenStale(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	tokens := &memTokens{csrf: map[int64]string{3000000001: "stale-token-0123456789"}}
	c.Tokens = tokens
	f.override["POST /api/v2/user_favourites/toggle"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-CSRF-Token") != "00000000-0000-4000-8000-000000000000" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":106,"message":"invalid csrf"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}
	if err := c.ToggleFavorite(context.Background(), 1000000001); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v2/user_favourites/toggle") != 2 || f.count("GET /") != 1 || tokens.drops != 1 {
		t.Fatalf("toggle hits %d, home hits %d, drops %d", f.count("POST /api/v2/user_favourites/toggle"), f.count("GET /"), tokens.drops)
	}
	if tokens.csrf[3000000001] != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("fresh token not cached: %q", tokens.csrf[3000000001])
	}
	// A fresh client finds the good token cached and loads no page.
	next := New()
	next.Base, next.Tokens = c.Base, tokens
	loggedIn(next)
	if err := next.ToggleFavorite(context.Background(), 1000000001); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /") != 1 {
		t.Fatalf("cached CSRF token not reused: %d home hits", f.count("GET /"))
	}
}

func TestBrowserHeadersAreConsistent(t *testing.T) {
	f, c := newFake(t)
	if _, err := c.Search(context.Background(), SearchParams{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	h := f.headers["GET /catalog"]
	if !strings.Contains(h.Get("User-Agent"), "Chrome/140") || !strings.Contains(h.Get("Sec-Ch-Ua"), `"Google Chrome";v="140"`) {
		t.Fatalf("user agent and client hints disagree: %q / %q", h.Get("User-Agent"), h.Get("Sec-Ch-Ua"))
	}
	if !strings.Contains(h.Get("User-Agent"), "Linux") || h.Get("Sec-Ch-Ua-Platform") != `"Linux"` {
		t.Fatalf("platform mismatch: %q / %q", h.Get("User-Agent"), h.Get("Sec-Ch-Ua-Platform"))
	}
	if h.Get("Sec-Fetch-Mode") != "navigate" {
		t.Fatalf("page load sent Sec-Fetch-Mode %q", h.Get("Sec-Fetch-Mode"))
	}
	c.Base = "https://www.vinted.fr"
	if !strings.HasPrefix(c.acceptLanguage(), "fr-FR") {
		t.Fatalf("vinted.fr language %q", c.acceptLanguage())
	}
	c.Base = "https://www.vinted.co.uk"
	if !strings.HasPrefix(c.acceptLanguage(), "en-GB") {
		t.Fatalf("vinted.co.uk language %q", c.acceptLanguage())
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Retry-After", "120")
	if d := retryAfter(h, now); d != 2*time.Minute {
		t.Fatalf("seconds: %v", d)
	}
	h.Set("Retry-After", now.Add(3*time.Hour).Format(http.TimeFormat))
	if d := retryAfter(h, now); d != 3*time.Hour {
		t.Fatalf("date: %v", d)
	}
	h.Set("Retry-After", "99999999")
	if d := retryAfter(h, now); d != maxRetryAfter {
		t.Fatalf("cap: %v", d)
	}
}
