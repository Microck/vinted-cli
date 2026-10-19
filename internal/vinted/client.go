// Package vinted is the only place that knows Vinted's unofficial API: hosts,
// paths, headers, cookies, response shapes and the server-rendered pages that
// stand in for missing JSON endpoints. Everything above it works with the
// normalized types in types.go, so an upstream change is fixed here and only here.
package vinted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultBase is the site used when neither the profile nor VINTED_BASE_URL names one.
const DefaultBase = "https://www.vinted.com"

type Client struct {
	// Base is the Vinted site the profile belongs to (https://www.vinted.fr,
	// https://www.vinted.es, ...). Sessions are per domain.
	Base string
	HTTP *http.Client
	// Debug, when non-nil, receives one line per request and response with
	// secrets redacted.
	Debug io.Writer
	// Notice receives one-off human hints.
	Notice func(string)
	// Account is the logged-in session; nil runs on a guest jar.
	Account Account
	// Gate guards every request (cooldown, pacing, budget); see Gate.
	Gate Gate
	// Tokens caches the CSRF token and guest jar across processes; nil keeps
	// them for this process only.
	Tokens TokenCache
	// PauseSessions, when set, stops the session engine's requests and
	// renewals for the site for d after a block, whether or not this client
	// is logged in: another profile's session renews from the same egress.
	PauseSessions func(ctx context.Context, d time.Duration) error

	mu            sync.Mutex
	redactStrings []string
	anon          *guest
	csrf          string
	csrfCached    bool   // csrf came from Tokens, not from a page this process loaded
	proxy         string // the proxy every request goes through, without credentials
}

// New builds a client with the base URL from VINTED_BASE_URL (tests point it
// at a fake server) and a 20 s per-request timeout. The one http.Client keeps
// its connections alive, so a command's requests reuse one TLS session the way
// a browser tab does. It sends no CLI version: Vinted only answers browser user
// agents (see browserUA).
func New() *Client {
	c := &Client{
		HTTP: &http.Client{Timeout: 20 * time.Second},
		Base: envOr("VINTED_BASE_URL", DefaultBase),
	}
	// send follows redirects itself, the same way for the guest jar and the
	// Account, so cookies never travel to another host.
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

// Redact registers a secret that must never appear in debug output or error bodies.
func (c *Client) Redact(secret string) {
	if secret == "" {
		return
	}
	c.mu.Lock()
	c.redactStrings = append(c.redactStrings, secret)
	c.mu.Unlock()
}

// redactCookie registers a cookie value for redaction. Short values are
// flags and ids ("1", "true", a locale), not secrets; redacting them would
// mangle every URL and body that happens to contain them.
func (c *Client) redactCookie(v string) {
	if len(v) >= 16 {
		c.Redact(v)
	}
}

func (c *Client) redact(s string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.redactStrings {
		s = strings.ReplaceAll(s, r, "[REDACTED]")
	}
	return s
}

type request struct {
	method string
	path   string
	query  url.Values
	body   any
	// auth requires the logged-in Session; without it the call fails with
	// KindAuth instead of silently running anonymously.
	auth bool
	// csrf adds the X-CSRF-Token header the web app sends on mutations.
	csrf bool
	// html asks for a server-rendered page instead of JSON.
	html bool
	// noToken sends the guest jar as is, without minting a fresh guest
	// token first (the bootstrap itself).
	noToken bool
	// guest overrides which guest jar is sent (the bootstrap).
	guest *guest
	// gateway sends the call to Vinted's API gateway (api.www.vinted.com,
	// api.vinted.fr, ...) instead of the site, the host the web app uses
	// for the newer services (saved searches, shipping estimation).
	gateway bool
	// raw is a pre-encoded body (multipart uploads) sent with contentType
	// instead of JSON-encoding body.
	raw         []byte
	contentType string
	// header carries extra request headers the web app sends on one call.
	header http.Header
}

// response is what send read off the wire.
type response struct {
	status int
	header http.Header
	raw    []byte
	// csrfCached is set when the request carried a CSRF token an earlier
	// command cached rather than one this process scraped.
	csrfCached bool
}

// do performs one call, decodes JSON into out (may be nil) and classifies
// failures into *Error so the CLI can map them to exit codes. It returns the
// raw body for HTML callers. A 401 is retried once after renewing the access
// token (the Account's, or a new guest token), since Vinted's tokens live
// about an hour, and a 403 on a cached CSRF token once with a freshly scraped
// one. A block is never retried: it records the cooldown, pauses the
// Account, and ends the command.
func (c *Client) do(ctx context.Context, r request, out any) ([]byte, error) {
	endpoint := r.method + " " + r.path
	g := r.guest
	if c.Account == nil && g == nil {
		if r.auth {
			return nil, &Error{Kind: KindAuth, Msg: "this command needs a logged-in profile. Run `vinted auth login`"}
		}
		var err error
		if g, err = c.guestJar(ctx); err != nil {
			return nil, err
		}
	}
	resp, err := c.send(ctx, r, g)
	if err != nil {
		return nil, err
	}
	if isBlock(resp.status, resp.raw) {
		return resp.raw, c.blocked(ctx, endpoint, resp)
	}
	retry := false
	switch {
	case resp.status == http.StatusUnauthorized && !r.noToken:
		if c.Account != nil {
			// The renewal is a request to Vinted of its own: gate it.
			if c.Gate != nil {
				if err := c.Gate.Wait(ctx); err != nil {
					return nil, err
				}
			}
			err = c.Account.Renew(ctx)
		} else {
			err = g.bootstrap(ctx)
		}
		if err != nil {
			return nil, err
		}
		retry = true
	case resp.status == http.StatusForbidden && resp.csrfCached:
		// The cached token belonged to an older login; load the current one.
		c.dropCSRF()
		retry = true
	}
	if retry {
		if resp, err = c.send(ctx, r, g); err != nil {
			return nil, err
		}
		if isBlock(resp.status, resp.raw) {
			return resp.raw, c.blocked(ctx, endpoint, resp)
		}
	}
	raw, status := resp.raw, resp.status
	if status < 200 || status >= 300 {
		return raw, c.statusError(endpoint, status, raw)
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, &Error{Kind: KindAPIChanged, Status: status, Endpoint: endpoint, Body: "decode: " + err.Error() + "; body: " + c.snippet(raw), Msg: "vinted returned a response the cli does not understand"}
		}
	}
	return raw, nil
}

// guestJar is the anonymous jar public reads run on without an Account. It
// comes from Tokens while its token is fresh and is otherwise minted from
// the home page and cached for the next command.
func (c *Client) guestJar(ctx context.Context) (*guest, error) {
	c.mu.Lock()
	g := c.anon
	c.mu.Unlock()
	if g != nil {
		return g, nil
	}
	g = &guest{client: c, cookies: map[string]string{}}
	if c.Tokens != nil {
		g.onChange = func(jar map[string]string, exp time.Time) {
			if err := c.Tokens.SaveGuest(jar, exp); err != nil {
				c.debugf("! could not cache the guest session: %v", err)
			}
		}
		if cached := c.Tokens.Guest(); cached[AccessCookie] != "" {
			for k, v := range cached {
				c.redactCookie(v)
				g.cookies[k] = v
			}
			g.expires = jwtExpiry(cached[AccessCookie])
		}
	}
	if err := g.ensure(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.anon == nil {
		c.anon = g
	}
	g = c.anon
	c.mu.Unlock()
	return g, nil
}

// maxRedirects bounds the same-host redirects send follows.
const maxRedirects = 5

func (c *Client) send(ctx context.Context, r request, g *guest) (response, error) {
	endpoint := r.method + " " + r.path
	base := c.Base
	if r.gateway {
		base = c.gatewayBase()
	}
	u := base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	body := r.raw
	contentType := r.contentType
	if r.raw == nil && r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return response{}, err
		}
		body, contentType = b, "application/json"
	}
	if g != nil && !r.noToken {
		if err := g.ensure(ctx); err != nil {
			return response{}, err
		}
	}
	header := http.Header{}
	c.browserHeaders(header, r.method, r.html, r.gateway)
	if !r.html {
		header.Set("Platform", "web")
	}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if r.gateway {
		// The gateway answers 400 BAD_REQUEST without a Locale header.
		header.Set("Locale", "en-US")
	}
	for k, v := range r.header {
		header[k] = v
	}
	var out response
	if r.csrf {
		tok, cached, err := c.csrfToken(ctx)
		if err != nil {
			return response{}, err
		}
		header.Set("X-CSRF-Token", tok)
		out.csrfCached = cached
	}
	method := r.method
	for hop := 0; ; hop++ {
		if g != nil {
			if h := g.header(); h != "" {
				header.Set("Cookie", h)
			}
		}
		// The gate goes last, right before the wire: the guest token and CSRF
		// page above are requests of their own and have already passed it.
		if c.Gate != nil {
			if err := c.Gate.Wait(ctx); err != nil {
				return response{}, err
			}
		}
		c.debugf("> %s %s", method, c.redact(u))
		status, respHeader, raw, err := c.transport(ctx, endpoint, method, u, header, body)
		if err != nil {
			return response{}, err
		}
		c.debugf("< %d %s (%d bytes)", status, endpoint, len(raw))
		out.status, out.header, out.raw = status, respHeader, raw
		// A block page's cookies (a fresh datadome flag) must not replace the jar.
		block := isBlock(status, raw)
		if g != nil && !block {
			g.absorb((&http.Response{Header: respHeader}).Cookies())
		}
		next, ok := redirectTarget(u, method, status, respHeader)
		if block || !ok || hop == maxRedirects {
			return out, nil
		}
		u, method, body = next, http.MethodGet, nil
		header.Del("Content-Type")
		header.Del("Origin")
	}
}

// redirectTarget is where a same-host redirect of a GET or HEAD leads. Other
// redirects are answered as they are, so cookies never follow a redirect to
// another host and a mutation is never silently replayed as another request.
func redirectTarget(current, method string, status int, h http.Header) (string, bool) {
	if status < 300 || status > 399 || status == http.StatusNotModified || (method != http.MethodGet && method != http.MethodHead) {
		return "", false
	}
	loc := h.Get("Location")
	if loc == "" {
		return "", false
	}
	from, err := url.Parse(current)
	if err != nil {
		return "", false
	}
	to, err := from.Parse(loc)
	if err != nil || to.Host != from.Host || to.Scheme != from.Scheme {
		return "", false
	}
	return to.String(), true
}

// transport puts one request on the wire: through the Account, which attaches
// the account's cookies, or directly with the guest jar's.
func (c *Client) transport(ctx context.Context, endpoint, method, u string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	if c.Account != nil {
		return c.Account.Send(ctx, method, u, header, body)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = header.Clone()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, c.netError(endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return 0, nil, nil, c.netError(endpoint, err)
	}
	return resp.StatusCode, resp.Header, raw, nil
}

var csrfRe = regexp.MustCompile(`CSRF_TOKEN\\?"\s*:\s*\\?"([0-9A-Za-z_\-=+/]{16,})`)

// csrfToken returns the web app's CSRF token, which Vinted bakes into the
// page config and checks on every mutating /api/v2 call. It is scraped from
// the home page at most once per client and cached in Tokens per account, so
// a mutation does not cost an extra page load each time. cached reports a
// token from Tokens, which may be stale.
func (c *Client) csrfToken(ctx context.Context) (tok string, cached bool, err error) {
	c.mu.Lock()
	tok, cached = c.csrf, c.csrfCached
	c.mu.Unlock()
	if tok != "" {
		return tok, cached, nil
	}
	user := c.csrfUser()
	if c.Tokens != nil {
		if tok = c.Tokens.CSRF(user); tok != "" {
			c.mu.Lock()
			c.csrf, c.csrfCached = tok, true
			c.mu.Unlock()
			return tok, true, nil
		}
	}
	raw, err := c.do(ctx, request{method: http.MethodGet, path: "/", html: true}, nil)
	if err != nil {
		return "", false, err
	}
	tok = scrapeCSRF(raw)
	if tok == "" {
		return "", false, &Error{Kind: KindAPIChanged, Endpoint: "GET /", Msg: "could not find vinted's CSRF token in the home page"}
	}
	c.mu.Lock()
	c.csrf, c.csrfCached = tok, false
	c.mu.Unlock()
	if c.Tokens != nil {
		if err := c.Tokens.SaveCSRF(user, tok); err != nil {
			c.debugf("! could not cache the CSRF token: %v", err)
		}
	}
	return tok, false, nil
}

// csrfUser is the account the CSRF token is cached under; 0 for a guest.
func (c *Client) csrfUser() int64 {
	if c.Account != nil {
		return c.Account.UserID()
	}
	return 0
}

func (c *Client) dropCSRF() {
	c.mu.Lock()
	c.csrf, c.csrfCached = "", false
	c.mu.Unlock()
	if c.Tokens != nil {
		if err := c.Tokens.DropCSRF(c.csrfUser()); err != nil {
			c.debugf("! could not drop the cached CSRF token: %v", err)
		}
	}
}

// scrapeCSRF looks in the raw HTML first, then in the decoded flight stream,
// where the page config may straddle two script chunks.
func scrapeCSRF(page []byte) string {
	for _, src := range [][]byte{page, rscStream(page)} {
		if m := csrfRe.FindSubmatch(src); m != nil {
			return string(m[1])
		}
	}
	if m := csrfMetaRe.FindSubmatch(page); m != nil {
		return string(m[1])
	}
	return ""
}

var csrfMetaRe = regexp.MustCompile(`<meta[^>]+name="csrf-token"[^>]+content="([^"]+)"`)

func (c *Client) debugf(format string, args ...any) {
	if c.Debug != nil {
		fmt.Fprintf(c.Debug, format+"\n", args...)
	}
}

func (c *Client) snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if isHTML(raw) {
		s = "(html page, " + fmt.Sprint(len(raw)) + " bytes)"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return c.redact(s)
}

func isHTML(raw []byte) bool {
	head := bytes.TrimSpace(raw)
	if len(head) > 512 {
		head = head[:512]
	}
	head = bytes.ToLower(head)
	return bytes.HasPrefix(head, []byte("<!doctype html")) || bytes.HasPrefix(head, []byte("<html"))
}

func (c *Client) netError(endpoint string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	c.mu.Lock()
	proxy := c.proxy
	c.mu.Unlock()
	via := ""
	if proxy != "" {
		via = " through the proxy " + proxy + " (the cli never falls back to a direct connection)"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Kind: KindNetwork, Endpoint: endpoint, Retryable: true, Msg: "request to vinted" + via + " timed out. Try again or check your network"}
	}
	return &Error{Kind: KindNetwork, Endpoint: endpoint, Retryable: true, Msg: "could not reach vinted" + via + ": " + c.redact(err.Error())}
}

// apiEnvelope is Vinted's error body: {"code":115,"message":"...","message_code":"user_verification_required"}.
// Validation failures (code 99) list the offending fields in errors. The
// gateway's codes are strings ("BAD_REQUEST"), which read as 0 here; the raw
// body always reaches the Error's Body.
type apiEnvelope struct {
	Code        flexInt                           `json:"code"`
	Message     lenient[string]                   `json:"message"`
	MessageCode lenient[string]                   `json:"message_code"`
	Errors      lenient[[]lenient[apiFieldError]] `json:"errors"`
}

// apiFieldError is one entry of a validation envelope. Value is read as a
// message or a list of messages; any other shape is shown as JSON.
type apiFieldError struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

// fieldErrors renders a validation envelope's per-field messages.
func (env apiEnvelope) fieldErrors() string {
	parts := make([]string, 0, len(env.Errors.V))
	for _, le := range env.Errors.V {
		fe := le.V
		value := fieldErrorText(fe.Value)
		switch {
		case fe.Field != "" && value != "":
			parts = append(parts, fe.Field+": "+value)
		case value != "":
			parts = append(parts, value)
		case fe.Field != "":
			parts = append(parts, fe.Field)
		case le.Raw != nil:
			parts = append(parts, string(le.Raw))
		}
	}
	return strings.Join(parts, "; ")
}

// fieldErrorText reads a validation message given as a string or a list of
// strings, and shows any other shape as its JSON.
func fieldErrorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, ", ")
	}
	return string(raw)
}

// statusError classifies a non-2xx answer from Vinted itself. Blocks never
// get here: do hands them to blocked first, so a 403 left over is one of
// Vinted's own permission errors.
func (c *Client) statusError(endpoint string, status int, raw []byte) error {
	e := &Error{Status: status, Endpoint: endpoint, Body: c.snippet(raw)}
	var env apiEnvelope
	_ = json.Unmarshal(raw, &env)
	code, msg, msgCode := int(env.Code), env.Message.V, env.MessageCode.V
	e.APICode = code
	switch {
	case msgCode == "user_verification_required" || code == 115:
		e.Kind = KindAuth
		// The web app answers code 115 by sending the user to this page.
		e.Msg = "vinted requires this account to be verified before it can use this feature. Open " + c.Base + "/users/verification while logged in, complete the verification it asks for, then retry"
		e.Suggest = []string{"open " + c.Base + "/users/verification"}
	case status == http.StatusUnauthorized || msgCode == "invalid_authentication_token":
		e.Kind = KindAuth
		e.Msg = "vinted rejected the session. Run `vinted auth login` again"
	case status == http.StatusForbidden:
		e.Kind = KindGeneric
		e.Msg = "vinted refused the request (HTTP 403)"
		if msg != "" {
			e.Msg = "vinted refused the request: " + msg
		}
	case status == http.StatusNotFound || status == http.StatusGone:
		e.Kind = KindNotFound
		e.Msg = "vinted has nothing at " + endpoint
		if msg != "" {
			e.Msg = msg
		}
	case status >= 500:
		e.Kind = KindGeneric
		e.Retryable = true
		e.Msg = fmt.Sprintf("vinted answered HTTP %d. Try again later", status)
	case msgCode == "validation_error" && len(env.Errors.V) > 0:
		// The request's own fields were wrong: the caller can fix them.
		e.Kind = KindUsage
		e.Msg = "vinted rejected the input: " + env.fieldErrors()
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status == http.StatusConflict:
		e.Kind = KindGeneric
		e.Msg = fmt.Sprintf("vinted refused the request (HTTP %d)", status)
		if msg != "" {
			e.Msg = msg
		}
	default:
		e.Kind = KindAPIChanged
		e.Msg = fmt.Sprintf("unexpected HTTP %d from vinted", status)
	}
	return e
}

// getJSON is the common read path against /api/v2.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, auth bool, out any) error {
	_, err := c.do(ctx, request{method: http.MethodGet, path: path, query: q, auth: auth}, out)
	return err
}

// getPage fetches a server-rendered page.
func (c *Client) getPage(ctx context.Context, path string, q url.Values) ([]byte, error) {
	return c.do(ctx, request{method: http.MethodGet, path: path, query: q, html: true}, nil)
}

// gatewayBase is the API gateway origin for the site in Base, resolved the
// way the web app does: api.<domain>, except vinted.com, whose gateway is
// api.www.vinted.com. Hosts that are not Vinted (tests, proxies) go through
// the site's transparent proxy path, as the web app does off-domain.
func (c *Client) gatewayBase() string {
	base := strings.TrimRight(c.Base, "/")
	u, err := url.Parse(base)
	if err != nil {
		return base + "/web/gateway"
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	switch {
	case host == "vinted.com":
		return "https://api.www.vinted.com"
	case strings.HasPrefix(host, "vinted."):
		return "https://api." + host
	}
	return base + "/web/gateway"
}
