package vinted

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Microck/galleton/cookiefile"
)

const (
	// AccessCookie is the short-lived (about an hour) JWT every request carries.
	AccessCookie = "access_token_web"
	// RefreshCookie is the HttpOnly week-long JWT that mints new access tokens.
	// It is single use: every renewal returns a new one, and sending a spent
	// one again makes Vinted revoke the whole login.
	RefreshCookie = "refresh_token_web"
)

// Account is the logged-in session. Its cookies stay out of this process
// (login only reads the export and hands it over):
// the CLI implements Account with the Galleton session engine, the only
// party that holds the cookies, renews the access token through
// RefreshPath, and keeps whatever Vinted sets in return. One owner means a
// spent refresh token is never sent twice.
type Account interface {
	// Send performs one request with the account's cookies attached. It is
	// never retried, here or by the implementation. Failures are *Error.
	Send(ctx context.Context, method, rawURL string, header http.Header, body []byte) (status int, respHeader http.Header, respBody []byte, err error)
	// Renew renews the access token now. Requests renew on their own before
	// expiry; this is for a 401 and for `vinted auth refresh`.
	Renew(ctx context.Context) error
	// UserID is the account's id, recorded at login.
	UserID() int64
}

// CookieExport is a browser cookie export narrowed to the Vinted sites.
type CookieExport struct {
	Cookies []cookiefile.Cookie
	// Site is the Vinted site the tokens were issued for (www.vinted.fr),
	// empty when the export does not say (a bare Cookie header) or holds
	// logins for several sites (see Sites).
	Site string
	// Sites are all the Vinted sites the export holds a login for.
	Sites []string
}

// AccessExpiry is the expiry of the access token among cookies (one site's,
// from For), zero when unknown.
func AccessExpiry(cookies []cookiefile.Cookie) time.Time {
	for _, c := range cookies {
		if c.Name == AccessCookie && c.Value != "" {
			return jwtExpiry(c.Value)
		}
	}
	return time.Time{}
}

// ReadCookieExport reads cookies exported from a logged-in browser tab with
// Galleton's parser: a Netscape cookies.txt, a Cookie-Editor, EditThisCookie,
// DevTools or Playwright JSON list (bare or wrapped as {"url", "cookies"}),
// or a Cookie header. A whole-browser export is narrowed to the Vinted sites
// and extraHosts while parsing.
func ReadCookieExport(r io.Reader, extraHosts ...string) (CookieExport, error) {
	hosts := append([]string{}, extraHosts...)
	for _, base := range allSites() {
		hosts = append(hosts, Hosts(base)...)
	}
	cookies, _, err := cookiefile.Read(r, cookiefile.Auto, hosts...)
	if err != nil {
		return CookieExport{}, err
	}
	out := CookieExport{Cookies: cookies}
	// The site comes from the refresh tokens, else from the access tokens:
	// a whole-browser export can hold logins for several sites, and their
	// tokens must not be mixed.
	for _, name := range []string{RefreshCookie, AccessCookie} {
		for _, c := range cookies {
			if c.Name != name || c.Domain == "" {
				continue
			}
			if site := normalizeSite(c.Domain); isVintedHost(strings.TrimPrefix(site, "www.")) && !slices.Contains(out.Sites, site) {
				out.Sites = append(out.Sites, site)
			}
		}
		if len(out.Sites) > 0 {
			break
		}
	}
	if !slices.ContainsFunc(cookies, func(c cookiefile.Cookie) bool { return c.Name == RefreshCookie }) {
		return CookieExport{}, errors.New("no " + RefreshCookie + " cookie found in the input; export all cookies of a logged-in vinted tab (the cookie is HttpOnly, so copy it with a cookie extension, not from the console)")
	}
	slices.Sort(out.Sites)
	if len(out.Sites) == 1 {
		out.Site = out.Sites[0]
	}
	return out, nil
}

// For keeps the cookies a site's origins may receive. Cookies without a
// domain (a Cookie header) belong to whichever site they are imported for.
func (e CookieExport) For(base string) []cookiefile.Cookie {
	hosts := Hosts(base)
	var out []cookiefile.Cookie
	for _, c := range e.Cookies {
		d := strings.TrimPrefix(strings.ToLower(c.Domain), ".")
		keep := d == ""
		for _, h := range hosts {
			if h == d || !c.HostOnly && strings.HasSuffix(h, "."+d) {
				keep = true
			}
		}
		if keep {
			out = append(out, c)
		}
	}
	return out
}

// allSites are the bases of every Vinted site, www.vinted.com first.
func allSites() []string {
	out := []string{DefaultBase}
	for host := range siteLanguages {
		out = append(out, "https://www."+host)
	}
	return out
}

// normalizeSite turns a cookie domain (".vinted.fr") or a bare site
// ("vinted.fr", "https://www.vinted.fr/") into the host the web app answers
// on ("www.vinted.fr").
func normalizeSite(d string) string {
	d = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(d), "https://"), "http://")
	d = strings.ToLower(strings.Trim(strings.TrimPrefix(d, "."), "/"))
	if strings.HasPrefix(d, "vinted.") {
		d = "www." + d
	}
	return d
}

// NormalizeSite is normalizeSite for the CLI's --domain flag.
func NormalizeSite(d string) string { return normalizeSite(d) }

// claims are the JWT claims read here. exp is a NumericDate, which RFC 7519
// allows to be non-integer, so it is read as flexInt.
type claims struct {
	Exp flexInt `json:"exp"`
}

// jwtClaims reads an unverified JWT. Verification is not needed: the claims
// only schedule renewals.
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
	if exp := int64(jwtClaims(tok).Exp); exp != 0 {
		return time.Unix(exp, 0)
	}
	return time.Time{}
}
