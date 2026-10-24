package vinted

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// guest is the anonymous cookie jar all public reads run on, independent
// of the stored Account: even /api/v2 reads need an (anonymous) access token. Vinted's
// home page mints one; it is minted again rather than refreshed, so no
// refresh token is ever spent here.
type guest struct {
	client *Client

	mu      sync.Mutex
	cookies map[string]string
	expires time.Time
	// onChange receives the jar after the access token changed, so the
	// next command can reuse it.
	onChange func(cookies map[string]string, expires time.Time)
}

// fresh reports an access token valid for at least another minute.
func (g *guest) fresh() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cookies[AccessCookie] != "" && time.Until(g.expires) > time.Minute
}

// ensure mints a new guest token when the current one is about to expire.
func (g *guest) ensure(ctx context.Context) error {
	if g.fresh() {
		return nil
	}
	return g.bootstrap(ctx)
}

// bootstrap loads the home page, which sets access_token_web for a guest.
func (g *guest) bootstrap(ctx context.Context) error {
	g.mu.Lock()
	g.cookies = map[string]string{}
	g.expires = time.Time{}
	g.mu.Unlock()
	if _, err := g.client.do(ctx, request{method: http.MethodGet, path: "/", html: true, noToken: true, guest: g}, nil); err != nil {
		return err
	}
	g.mu.Lock()
	ok := g.cookies[AccessCookie] != ""
	g.mu.Unlock()
	if !ok {
		return &Error{Kind: KindAPIChanged, Endpoint: "GET /", Msg: "vinted did not hand out a guest session. Run `vinted auth login` to use your own"}
	}
	return nil
}

// absorb applies Set-Cookie headers from a response. Deletions are ignored:
// an error page clearing the token must not end the guest session early.
func (g *guest) absorb(cookies []*http.Cookie) {
	g.mu.Lock()
	changed := false
	for _, ck := range cookies {
		if ck.Value == "" || ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now())) {
			continue
		}
		if g.cookies[ck.Name] == ck.Value {
			continue
		}
		g.client.redactCookie(ck.Value)
		g.cookies[ck.Name] = ck.Value
		if ck.Name == AccessCookie {
			g.expires = jwtExpiry(ck.Value)
			changed = true
		}
	}
	var jar map[string]string
	exp := g.expires
	if changed && g.onChange != nil {
		jar = make(map[string]string, len(g.cookies))
		for k, v := range g.cookies {
			jar[k] = v
		}
	}
	onChange := g.onChange
	g.mu.Unlock()
	if jar != nil {
		onChange(jar, exp)
	}
}

// header renders the jar as a Cookie header, sorted for stable output.
func (g *guest) header() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, 0, len(g.cookies))
	for k := range g.cookies {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(g.cookies[k])
	}
	return b.String()
}
