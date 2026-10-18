package vinted

import (
	"encoding/json"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

// The account's cookies live in the Galleton session engine, not in the CLI.
// The engine renews them through POST /web/api/auth/refresh, keeps what each
// response sets and attaches them to every account request. This file is
// the engine's view of Vinted: one provider per site, with the origins its
// cookies may reach. The shape follows galleton's examples/adapters.vinted.json.

// RefreshPath is the web app's token renewal endpoint.
const RefreshPath = "/web/api/auth/refresh"

// MaxRequestBody bounds one account request sent through the engine: a photo
// upload of MaxPhotoBytes plus its multipart envelope.
const MaxRequestBody = MaxPhotoBytes + 1<<20

// maxResponseBody bounds one response read from Vinted, through the engine
// or directly. Catalog pages are about 4 MiB of HTML.
const maxResponseBody = 16 << 20

// Provider names the session engine's provider for a site: "vinted" for
// www.vinted.com, "vinted-fr" for www.vinted.fr, "vinted-co-uk" for
// www.vinted.co.uk, and "vinted-custom" for any other base (tests, a local
// mirror set with VINTED_BASE_URL).
func Provider(base string) string {
	host := siteHost(base)
	switch {
	case host == "vinted.com":
		return "vinted"
	case isVintedHost(host):
		return "vinted-" + strings.ReplaceAll(strings.TrimPrefix(host, "vinted."), ".", "-")
	}
	return "vinted-custom"
}

// siteHost is a base's host without "www.", lower case.
func siteHost(base string) string {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

type adapterResponse struct {
	ExpiresInPointer string `json:"expires_in_pointer"`
	RequireSetCookie bool   `json:"require_set_cookie"`
}

type blockSignature struct {
	Status         []int  `json:"status"`
	BodyContains   string `json:"body_contains,omitempty"`
	Header         string `json:"header,omitempty"`
	HeaderContains string `json:"header_contains,omitempty"`
	NotJSON        bool   `json:"not_json,omitempty"`
}

type adapterProvider struct {
	Name                   string            `json:"name"`
	Kind                   string            `json:"kind"`
	Origins                []string          `json:"origins"`
	RefreshURL             string            `json:"refresh_url"`
	RefreshMethod          string            `json:"refresh_method"`
	RefreshHeaders         map[string]string `json:"refresh_headers"`
	BodyFormat             string            `json:"body_format"`
	Response               adapterResponse   `json:"response"`
	RefreshIntervalSeconds int               `json:"refresh_interval_seconds"`
	RefreshBeforeSeconds   int               `json:"refresh_before_seconds"`
	CookieDomain           string            `json:"cookie_domain,omitempty"`
	ProtectedCookies       []string          `json:"protected_cookies"`
	BlockSignatures        []blockSignature  `json:"block_signatures"`
	MinIntervalMS          int               `json:"min_interval_ms"`
	KeepAlive              bool              `json:"keep_alive"`
	MaxRequestBodyBytes    int               `json:"max_request_body_bytes"`
	MaxResponseBodyBytes   int               `json:"max_response_body_bytes"`
	ProxyEnv               string            `json:"proxy_env,omitempty"`
	AllowHTTPLoopback      bool              `json:"allow_http_loopback,omitempty"`
}

// Adapter renders the session engine's configuration: a provider for every
// Vinted site, plus base when it is not one of them (VINTED_BASE_URL). With
// proxyEnv set, every renewal and account request leaves through the proxy
// in that environment variable and never directly. interval is the CLI's
// request gap, which the engine keeps between its own calls too, renewals
// included, since those do not pass the CLI's gate.
func Adapter(base, proxyEnv string, interval time.Duration) ([]byte, error) {
	bases := make([]string, 0, len(siteLanguages)+2)
	bases = append(bases, DefaultBase)
	for host := range siteLanguages {
		bases = append(bases, "https://www."+host)
	}
	sort.Strings(bases[1:])
	if Provider(base) == "vinted-custom" {
		bases = append(bases, strings.TrimRight(base, "/"))
	}
	providers := make([]adapterProvider, 0, len(bases))
	for _, b := range bases {
		providers = append(providers, siteProvider(b, proxyEnv, interval))
	}
	return json.MarshalIndent(map[string]any{"providers": providers}, "", "  ")
}

func siteProvider(base, proxyEnv string, interval time.Duration) adapterProvider {
	c := &Client{Base: base}
	p := adapterProvider{
		Name:          Provider(base),
		Kind:          "http",
		Origins:       Origins(base),
		RefreshURL:    base + RefreshPath,
		RefreshMethod: "POST",
		// The headers the web app's own refresh fetch carries.
		RefreshHeaders: map[string]string{},
		BodyFormat:     "json",
		// The body's expires_in is the new access token's lifetime. Both
		// token cookies rotate in Set-Cookie; the refresh token is single
		// use, so a renewal that does not replace both stops the session.
		Response:               adapterResponse{ExpiresInPointer: "/expires_in", RequireSetCookie: true},
		RefreshIntervalSeconds: 3600,
		RefreshBeforeSeconds:   300,
		// Only a renewal may set the tokens: a page handing out guest tokens
		// must not replace the account's.
		ProtectedCookies: []string{AccessCookie, RefreshCookie},
		// A Datadome or Cloudflare challenge, or any 403 that is not one of
		// Vinted's JSON errors, means "come back later", not "log in again".
		BlockSignatures: []blockSignature{
			{Status: []int{403}, BodyContains: "captcha-delivery.com"},
			{Status: []int{403, 503}, Header: "Cf-Mitigated", HeaderContains: "challenge"},
			{Status: []int{403}, NotJSON: true},
		},
		MinIntervalMS:        int(interval / time.Millisecond),
		KeepAlive:            true,
		MaxRequestBodyBytes:  MaxRequestBody,
		MaxResponseBodyBytes: maxResponseBody,
		ProxyEnv:             proxyEnv,
	}
	h := map[string][]string{}
	c.browserHeaders(h, "POST", false, false)
	for k, v := range h {
		p.RefreshHeaders[k] = v[0]
	}
	p.RefreshHeaders["Platform"] = "web"
	host := siteHost(base)
	switch {
	case host == "vinted.com":
		// www.vinted.com and its gateway api.www.vinted.com share the tokens
		// Vinted sets on .www.vinted.com; Datadome's .vinted.com cookie stays
		// on the origin that received it.
		p.CookieDomain = ".www.vinted.com"
	case isVintedHost(host):
		// National sites run their gateway on api.vinted.xx, which only
		// shares cookies set on the site's registrable domain.
		p.CookieDomain = "." + host
	default:
		// A local stand-in: one origin, plain HTTP allowed on loopback only,
		// no pacing beyond the CLI's own request gate.
		p.MinIntervalMS = 0
		if u, err := url.Parse(base); err == nil && u.Scheme == "http" {
			if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
				p.AllowHTTPLoopback = true
			}
		}
	}
	return p
}

// Origins are the origins of a site whose requests carry the account's
// cookies: the site and its API gateway.
func Origins(base string) []string {
	base = strings.TrimRight(base, "/")
	c := &Client{Base: base}
	gw := c.gatewayBase()
	if strings.HasPrefix(gw, base) {
		return []string{base}
	}
	return []string{base, gw}
}

// Hosts are the host names of a site's origins, for filtering a whole
// browser cookie export down to the cookies this site may use.
func Hosts(base string) []string {
	var out []string
	for _, o := range Origins(base) {
		if u, err := url.Parse(o); err == nil {
			out = append(out, strings.ToLower(u.Hostname()))
		}
	}
	return out
}
