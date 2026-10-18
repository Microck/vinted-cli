package vinted

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AccessCookie is the short-lived (about an hour) JWT every request carries.
	AccessCookie = "access_token_web"
	// RefreshCookie is the HttpOnly week-long JWT that mints new access tokens.
	// Users export it with a cookie extension, not from the console.
	RefreshCookie = "refresh_token_web"
)

// persistedCookies are the jar entries whose rotation is worth writing to
// disk: the two tokens and Datadome's clearance. The rest (Cloudflare's
// __cf_bm, the Rails session) is refreshed in memory and saved alongside.
var persistedCookies = map[string]bool{AccessCookie: true, RefreshCookie: true, "datadome": true}

// Session is the browser cookie jar imported from a logged-in Vinted tab.
// Vinted authenticates the web app purely with cookies: access_token_web is
// refreshed through POST /web/api/auth/refresh with refresh_token_web, and
// every response may re-issue cookies.
//
// The refresh token is single use: every refresh returns a new one, and
// sending a spent one again makes Vinted revoke the whole login (every token
// of that session id). So refreshes are single-flight within the process,
// the rotated jar is persisted before the new token is used, and Store
// serialises refreshes across processes sharing one stored profile.
type Session struct {
	Cookies map[string]string
	Expires time.Time
	// OnRotate receives the cookies that changed whenever a token or Datadome
	// cookie changes outside a Store-guarded refresh, so the caller can merge
	// them into what it persisted. Only changed entries are passed: a
	// long-running process must never write back its own, possibly spent,
	// refresh token over one another process rotated since. expires is the
	// new access token expiry, zero when the access token did not change.
	OnRotate func(changed map[string]string, expires time.Time)
	// Store, when set, guards refreshes across processes; nil keeps the
	// refresh process-local.
	Store SessionStore

	client    *Client
	anonymous bool
	refreshMu sync.Mutex
	mu        sync.Mutex
	// staged collects cookie changes while a Store-guarded refresh runs; they
	// reach Cookies only once the Store has saved them, so memory never holds
	// a token pair whose predecessor is still the one on disk.
	staged map[string]string
	// unsaved is set when Vinted rotated the tokens but the Store could not
	// save them. The refresh token on disk and in memory is then spent, and
	// sending it again would revoke the login, so every later refresh fails
	// with this error instead.
	unsaved error
}

// SessionStore shares one persisted jar between processes (a watch timer and
// an interactive command). A refresh runs entirely under Lock: it Loads the
// stored jar, adopts a token another process already rotated instead of
// replaying a spent one, and Saves the new jar before unlocking.
type SessionStore interface {
	// Lock takes a cross-process, non-re-entrant lock on the stored jar.
	Lock(ctx context.Context) (unlock func(), err error)
	// Load returns the persisted jar; a nil map means the profile was logged out.
	Load() (cookies map[string]string, err error)
	// Save persists the jar; it is only called while locked.
	Save(cookies map[string]string, expires time.Time) error
}

// NewSession wraps an imported jar. Every cookie value long enough to be a
// secret is registered for redaction, and Expires is read from the access token.
func NewSession(c *Client, cookies map[string]string) *Session {
	jar := make(map[string]string, len(cookies))
	for k, v := range cookies {
		jar[k] = v
		c.redactCookie(v)
	}
	return &Session{Cookies: jar, Expires: jwtExpiry(jar[AccessCookie]), client: c}
}

// AccessToken returns the current access token, refreshing it 60 s before expiry.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	tok, exp := s.Cookies[AccessCookie], s.Expires
	s.mu.Unlock()
	if tok != "" && time.Until(exp) > 60*time.Second {
		return tok, nil
	}
	if err := s.refreshIfStale(ctx, tok); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cookies[AccessCookie], nil
}

// refreshIfStale refreshes unless another goroutine already replaced the token
// it saw while this one waited for the lock.
func (s *Session) refreshIfStale(ctx context.Context, seen string) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	current, exp := s.Cookies[AccessCookie], s.Expires
	s.mu.Unlock()
	if current != seen && current != "" && time.Until(exp) > 60*time.Second {
		return nil
	}
	return s.refreshLocked(ctx)
}

type refreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

// Refresh exchanges refresh_token_web for a new access token, exactly as the
// web app does when its token runs out.
func (s *Session) Refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refreshLocked(ctx)
}

func (s *Session) refreshLocked(ctx context.Context) error {
	if s.anonymous {
		// Anonymous tokens are cheaper to mint again than to refresh.
		return s.bootstrap(ctx)
	}
	if s.Store == nil {
		updates, err := s.exchange(ctx)
		if err != nil {
			return err
		}
		// OnRotate persists the spent-token replacement before anyone uses it.
		s.merge(updates, true)
		return nil
	}
	s.mu.Lock()
	unsaved := s.unsaved
	s.mu.Unlock()
	if unsaved != nil {
		return unsaved
	}
	unlock, err := s.Store.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	stored, err := s.Store.Load()
	if err != nil {
		return err
	}
	if stored == nil {
		return &Error{Kind: KindAuth, Msg: "the profile was logged out. Run `vinted auth login`"}
	}
	if s.adopt(stored) {
		return nil
	}
	s.mu.Lock()
	s.staged = map[string]string{}
	s.mu.Unlock()
	updates, err := s.exchange(ctx)
	if err != nil {
		// The refresh failed: keep what the responses set (Datadome,
		// Cloudflare), as any other request would.
		s.commitStaged()
		return err
	}
	// Persist first, adopt second: the rotated jar is built from the staged
	// changes and only replaces the in-memory one after Save succeeded.
	s.mu.Lock()
	for k, v := range updates {
		s.client.redactCookie(v)
		s.staged[k] = v
	}
	jar := make(map[string]string, len(s.Cookies)+len(s.staged))
	for k, v := range s.Cookies {
		jar[k] = v
	}
	for k, v := range s.staged {
		jar[k] = v
	}
	s.mu.Unlock()
	if err := s.Store.Save(jar, jwtExpiry(jar[AccessCookie])); err != nil {
		s.mu.Lock()
		s.staged = nil
		s.unsaved = &Error{Kind: KindAuth, Endpoint: "POST /web/api/auth/refresh", Msg: fmt.Sprintf("vinted rotated the session tokens but saving them failed (%v); the stored session is now spent and likely needs `vinted auth login` again", err)}
		unsaved = s.unsaved
		s.mu.Unlock()
		return unsaved
	}
	s.commitStaged()
	return nil
}

// commitStaged moves the changes staged during a Store-guarded refresh into
// the jar and ends staging. OnRotate is not called: the refresh saves the
// whole jar itself while holding the Store lock, which OnRotate may take.
func (s *Session) commitStaged() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.staged {
		s.Cookies[k] = v
		if k == AccessCookie {
			s.Expires = jwtExpiry(v)
		}
	}
	s.staged = nil
}

// exchange spends the refresh token and returns the new token pair.
func (s *Session) exchange(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	hasRefresh := s.Cookies[RefreshCookie] != ""
	s.mu.Unlock()
	if !hasRefresh {
		return nil, &Error{Kind: KindAuth, Msg: "the stored cookies have no " + RefreshCookie + ". Export the cookies again from a logged-in vinted tab and run `vinted auth login`"}
	}
	var out refreshResponse
	_, err := s.client.do(ctx, request{method: http.MethodPost, path: "/web/api/auth/refresh", body: struct{}{}, noToken: true, session: s}, &out)
	if err != nil {
		var ve *Error
		if errors.As(err, &ve) && (ve.Status == http.StatusBadRequest || ve.Status == http.StatusUnauthorized) {
			return nil, &Error{Kind: KindAuth, Status: ve.Status, Endpoint: ve.Endpoint, Body: ve.Body, Msg: "the stored vinted session has expired or was revoked. Log in on the website, export the cookies again and run `vinted auth login`"}
		}
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, &Error{Kind: KindAuth, Endpoint: "POST /web/api/auth/refresh", Msg: "vinted did not return a new access token. Run `vinted auth login` again"}
	}
	updates := map[string]string{AccessCookie: out.AccessToken}
	if out.RefreshToken != "" {
		updates[RefreshCookie] = out.RefreshToken
	}
	return updates, nil
}

// adopt reconciles the jar with the stored one under the Store lock. When
// another process already refreshed and its access token is still fresh, the
// stored jar replaces ours and adopt reports true (no refresh needed).
// Otherwise it only takes the stored refresh token, the one not yet spent.
func (s *Session) adopt(stored map[string]string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc := stored[AccessCookie]; acc != "" && acc != s.Cookies[AccessCookie] && time.Until(jwtExpiry(acc)) > 60*time.Second {
		jar := make(map[string]string, len(stored))
		for k, v := range stored {
			s.client.redactCookie(v)
			jar[k] = v
		}
		s.Cookies, s.Expires = jar, jwtExpiry(acc)
		return true
	}
	if r := stored[RefreshCookie]; r != "" && r != s.Cookies[RefreshCookie] {
		s.client.redactCookie(r)
		s.Cookies[RefreshCookie] = r
	}
	return false
}

func (s *Session) snapshot() (map[string]string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jar := make(map[string]string, len(s.Cookies))
	for k, v := range s.Cookies {
		jar[k] = v
	}
	return jar, s.Expires
}

// bootstrap mints anonymous tokens by loading the home page, which sets
// access_token_web and refresh_token_web for a guest.
func (s *Session) bootstrap(ctx context.Context) error {
	s.mu.Lock()
	s.Cookies = map[string]string{}
	s.Expires = time.Time{}
	s.mu.Unlock()
	_, err := s.client.do(ctx, request{method: http.MethodGet, path: "/", html: true, noToken: true, session: s}, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	ok := s.Cookies[AccessCookie] != ""
	s.mu.Unlock()
	if !ok {
		return &Error{Kind: KindAPIChanged, Endpoint: "GET /", Msg: "vinted did not hand out a guest session. Run `vinted auth login` to use your own"}
	}
	return nil
}

// absorb applies Set-Cookie headers from a response. Deletions are ignored:
// an error page clearing the token must not destroy the imported login.
// Neither may guest tokens: a page that does not see the login (Vinted's web
// server mints public-scope tokens whenever the refresh cookie is missing)
// must not replace the account's tokens and lose the refresh token for good.
func (s *Session) absorb(cookies []*http.Cookie) {
	loggedIn := !s.anonymous && s.UserID() != 0
	updates := map[string]string{}
	for _, ck := range cookies {
		if ck.Value == "" || ck.MaxAge < 0 || (!ck.Expires.IsZero() && ck.Expires.Before(time.Now())) {
			continue
		}
		if loggedIn && (ck.Name == AccessCookie || ck.Name == RefreshCookie) && jwtClaims(ck.Value).sub() == 0 {
			continue
		}
		updates[ck.Name] = ck.Value
	}
	if len(updates) > 0 {
		s.merge(updates, false)
	}
}

// merge updates the jar and hands the changed entries to OnRotate when a
// persisted cookie changed (or always, when force is set after an explicit
// refresh). While a Store-guarded refresh holds the store lock, changes are
// staged instead and OnRotate is skipped: the refresh saves the whole jar
// itself before adopting it, and OnRotate may take the same lock.
func (s *Session) merge(updates map[string]string, force bool) {
	s.mu.Lock()
	if s.staged != nil {
		for k, v := range updates {
			s.client.redactCookie(v)
			s.staged[k] = v
		}
		s.mu.Unlock()
		return
	}
	notify := force
	changed := map[string]string{}
	var exp time.Time
	for k, v := range updates {
		if s.Cookies[k] == v {
			continue
		}
		s.client.redactCookie(v)
		s.Cookies[k] = v
		changed[k] = v
		if persistedCookies[k] {
			notify = true
		}
		if k == AccessCookie {
			s.Expires = jwtExpiry(v)
			exp = s.Expires
		}
	}
	call := notify && len(changed) > 0 && s.OnRotate != nil
	s.mu.Unlock()
	if call {
		s.OnRotate(changed, exp)
	}
}

// cookieHeader renders the jar as a Cookie header, sorted for stable output.
func (s *Session) cookieHeader() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.Cookies))
	for k := range s.Cookies {
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
		b.WriteString(s.Cookies[k])
	}
	return b.String()
}

// UserID is the account id in the access (or refresh) token's sub claim;
// zero for a guest jar.
func (s *Session) UserID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range []string{AccessCookie, RefreshCookie} {
		if id := jwtClaims(s.Cookies[name]).sub(); id != 0 {
			return id
		}
	}
	return 0
}

type claims struct {
	Exp int64           `json:"exp"`
	Sub json.RawMessage `json:"sub"`
}

// sub accepts the subject as a JSON string (what Vinted sends) or a number.
func (c claims) sub() int64 {
	raw := strings.Trim(string(c.Sub), `"`)
	id, _ := strconv.ParseInt(raw, 10, 64)
	return id
}

// jwtClaims reads an unverified JWT. Verification is not needed: the claims
// only schedule refreshes and name the account.
func jwtClaims(tok string) claims {
	var out claims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return out
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func jwtExpiry(tok string) time.Time {
	if exp := jwtClaims(tok).Exp; exp != 0 {
		return time.Unix(exp, 0)
	}
	return time.Time{}
}

// ParseCookieExport reads cookies exported from a logged-in browser tab. It
// accepts a Netscape cookie file (Cookie-Editor, curl, browser exporters), a
// JSON array of {name,value,domain} objects (Cookie-Editor's JSON export), the
// same array wrapped as {"url":"https://www.vinted.com","cookies":[...]}, or a
// raw `a=b; c=d` header with or without a `Cookie:` prefix. host is the Vinted
// host the tokens were issued for (www.vinted.fr): the wrapper's url when it
// has one, else the token cookies' domain, empty when the input does not say.
func ParseCookieExport(r io.Reader) (cookies map[string]string, host string, err error) {
	data, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, "", err
	}
	trimmed := bytes.TrimSpace(data)
	cookies = map[string]string{}
	domains := map[string]string{}
	switch {
	case len(trimmed) > 0 && (trimmed[0] == '[' || trimmed[0] == '{'):
		if host, err = parseJSONCookies(trimmed, cookies, domains); err != nil {
			return nil, "", err
		}
	default:
		parseTextCookies(data, cookies, domains)
	}
	if cookies[AccessCookie] == "" && cookies[RefreshCookie] == "" {
		return nil, "", errors.New("no " + AccessCookie + " or " + RefreshCookie + " cookie found in the input; export all cookies of a logged-in vinted tab")
	}
	for _, name := range []string{RefreshCookie, AccessCookie} {
		if d := domains[name]; d != "" && host == "" {
			host = d
			break
		}
	}
	return cookies, host, nil
}

// parseJSONCookies fills cookies and their domains from a JSON export and
// returns the site host named by a wrapper's url, if any.
func parseJSONCookies(data []byte, cookies, domains map[string]string) (string, error) {
	type entry struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	}
	var list []entry
	host := ""
	if data[0] == '{' {
		var wrapped struct {
			URL     string  `json:"url"`
			Cookies []entry `json:"cookies"`
		}
		if err := json.Unmarshal(data, &wrapped); err != nil {
			return "", errors.New("the cookie JSON is not valid: " + err.Error())
		}
		list = wrapped.Cookies
		if u, err := url.Parse(strings.TrimSpace(wrapped.URL)); err == nil && u.Host != "" {
			host = normalizeHost(u.Hostname())
		}
	} else if err := json.Unmarshal(data, &list); err != nil {
		return "", errors.New("the cookie JSON is not valid: " + err.Error())
	}
	for _, e := range list {
		if e.Name == "" {
			continue
		}
		cookies[e.Name] = e.Value
		domains[e.Name] = normalizeHost(e.Domain)
	}
	return host, nil
}

func parseTextCookies(data []byte, cookies, domains map[string]string) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// Netscape format: domain, flag, path, secure, expiry, name, value.
		// HttpOnly cookies are prefixed "#HttpOnly_" but still tab-separated.
		if fields := strings.Split(line, "\t"); len(fields) >= 7 {
			name := fields[5]
			cookies[name] = strings.TrimSpace(fields[6])
			domains[name] = normalizeHost(strings.TrimPrefix(fields[0], "#HttpOnly_"))
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Header or pasted pairs: "Cookie: a=b; c=d" or "a=b; c=d".
		if len(line) >= 7 && strings.EqualFold(line[:7], "cookie:") {
			line = line[7:]
		}
		for _, pair := range strings.Split(line, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(pair), "=")
			if !found || strings.TrimSpace(name) == "" {
				continue
			}
			cookies[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
}

func normalizeHost(domain string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
}
