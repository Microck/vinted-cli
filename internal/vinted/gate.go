package vinted

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Gate is the policy every request to a Vinted host passes through right
// before it goes on the wire (internal/throttle implements it): a cooldown
// after a block, pacing between requests shared by every process, and a
// per-command request budget. A nil Gate sends unguarded, which only the API
// package's own tests do.
type Gate interface {
	// Wait returns once the next request may be sent, or the reason it must
	// not be (an active cooldown, an exhausted budget, ctx).
	Wait(ctx context.Context) error
	// Block records that the bot protection blocked a request and returns how
	// long every process must now stay away. retryAfter is the server's hint,
	// zero when it gave none.
	Block(cause string, retryAfter time.Duration) (time.Duration, error)
}

// TokenCache keeps what a command would otherwise load the home page for, so
// consecutive commands do not each fetch it again: the CSRF token per account
// (user 0 is the guest) and the guest cookie jar. Failures to save are not
// fatal; the next command scrapes again.
type TokenCache interface {
	CSRF(user int64) string
	SaveCSRF(user int64, token string) error
	DropCSRF(user int64) error
	Guest() map[string]string
	SaveGuest(cookies map[string]string, expires time.Time) error
}

// maxRetryAfter bounds a server's Retry-After so a broken or hostile value
// cannot lock the CLI out for good.
const maxRetryAfter = 7 * 24 * time.Hour

// isBlock reports whether a response is the bot protection refusing the
// client rather than Vinted answering: any rate limit, any Datadome or
// Cloudflare challenge page whatever its status, an error answer saying the
// session was blocked (the web app's "Se ha bloqueado tu sesión"), and a 403
// that is not one of Vinted's own JSON error envelopes (those are ordinary
// permission errors).
func isBlock(status int, raw []byte) bool {
	if status == http.StatusTooManyRequests || isBotWall(raw) {
		return true
	}
	if status >= 400 && bytes.Contains(raw, []byte("Se ha bloqueado")) {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	var env apiEnvelope
	if json.Unmarshal(raw, &env) == nil && (env.Code != 0 || env.Message != "" || env.MessageCode != "") {
		return false
	}
	return true
}

// isBotWall recognises the Datadome captcha and block pages (HTML, or the
// JSON {"url":"https://geo.captcha-delivery.com/..."} the API hosts send) and
// Cloudflare's challenge and block pages. The markers are specific to those
// pages: real Vinted pages also mention "datadome" and load Cloudflare's
// challenge-platform script.
func isBotWall(raw []byte) bool {
	for _, marker := range []string{
		"captcha-delivery.com",
		"<title>Just a moment...</title>",
		"cf_chl_opt",
		"Attention Required! | Cloudflare",
		"Sorry, you have been blocked",
	} {
		if bytes.Contains(raw, []byte(marker)) {
			return true
		}
	}
	return false
}

// retryAfter reads a Retry-After header in seconds or as an HTTP date.
func retryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

// blocked turns a block response into the error the user sees, after
// recording the cooldown that keeps every process away from the site. It is
// final: nothing retries a blocked request, since every retry extends the
// block and can get the account's session flagged.
func (c *Client) blocked(ctx context.Context, endpoint string, resp response) error {
	e := &Error{Kind: KindBlocked, Status: resp.status, Endpoint: endpoint, Body: c.snippet(resp.raw)}
	what := fmt.Sprintf("vinted's bot protection (Datadome/Cloudflare) blocked the request (HTTP %d on %s)", resp.status, endpoint)
	if resp.status == http.StatusTooManyRequests && !isBotWall(resp.raw) {
		what = "vinted rate-limited the request (HTTP 429 on " + endpoint + ")"
	}
	host := c.Host()
	if c.Gate == nil {
		e.Msg = what + ". Stop running vinted commands for a few hours. Open " + c.Base + " in your normal browser; if it shows a captcha or a block page, solve it there, and if the block persists re-export your cookies with `vinted auth login`"
		e.Suggest = BlockAdvice(c.Base, 0)
		return e
	}
	wait, err := c.Gate.Block(what, retryAfter(resp.header, time.Now()))
	e.Msg = what + ". Stop running vinted commands: the cli will not contact " + host + " for the next " + FormatWait(wait) + ", because every retry keeps the block going. Open " + c.Base + " in your normal browser; if it shows a captcha or a block page, solve it there, and if the block persists re-export your cookies with `vinted auth login`"
	if err != nil {
		e.Msg += fmt.Sprintf(" (the cooldown could not be saved: %v)", err)
	}
	// The session engine renews on its own schedule, outside the gate: pause
	// it too, so no renewal goes out while the site is blocking us.
	if c.PauseSessions != nil && wait > 0 {
		if err := c.PauseSessions(ctx, wait); err != nil {
			e.Msg += fmt.Sprintf(" (the session engine could not be paused: %v)", err)
		}
	}
	e.Suggest = BlockAdvice(c.Base, wait)
	return e
}

// BlockAdvice is the next-steps list printed under every block and cooldown
// error. wait is the time left; zero when unknown.
func BlockAdvice(base string, wait time.Duration) []string {
	first := "stop running vinted commands for a few hours"
	if wait > 0 {
		first = "stop running vinted commands and wait " + FormatWait(wait)
	}
	return []string{
		first,
		"open " + base + " in your normal browser",
		"vinted auth login (re-export cookies if the block persists)",
		"vinted cooldown (shows when the pause ends)",
	}
}

// FormatWait renders a remaining wait to the minute ("5h41m", "12m", "<1m").
func FormatWait(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "<1m"
	}
	s := d.String()
	return strings.TrimSuffix(s, "0s")
}

// Host is the site host the client talks to (www.vinted.fr), the key its
// cooldown and pacing are shared under.
func (c *Client) Host() string {
	if u, err := url.Parse(c.Base); err == nil && u.Host != "" {
		return u.Host
	}
	return c.Base
}

// Every request presents the same Chrome on Linux: the client hints must
// name the browser and platform the user agent claims, or the mismatch alone
// marks the client as a bot.
const (
	browserUA       = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	browserCHUA     = `"Chromium";v="140", "Not=A?Brand";v="24", "Google Chrome";v="140"`
	browserPlatform = `"Linux"`
	navigateAccept  = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
)

// siteLanguages is the Accept-Language a local browser sends to each national
// site, and with vinted.com the list of Vinted sites isVintedHost accepts.
// Unknown hosts (vinted.com, tests) get US English.
var siteLanguages = map[string]string{
	"vinted.fr":    "fr-FR,fr;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.be":    "fr-BE,fr;q=0.9,nl;q=0.8,en;q=0.7",
	"vinted.lu":    "fr-LU,fr;q=0.9,de;q=0.8,en;q=0.7",
	"vinted.es":    "es-ES,es;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.it":    "it-IT,it;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.de":    "de-DE,de;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.at":    "de-AT,de;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.nl":    "nl-NL,nl;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.pl":    "pl-PL,pl;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.pt":    "pt-PT,pt;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.lt":    "lt-LT,lt;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.lv":    "lv-LV,lv;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.ee":    "et-EE,et;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.cz":    "cs-CZ,cs;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.sk":    "sk-SK,sk;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.hu":    "hu-HU,hu;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.ro":    "ro-RO,ro;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.se":    "sv-SE,sv;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.fi":    "fi-FI,fi;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.dk":    "da-DK,da;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.gr":    "el-GR,el;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.hr":    "hr-HR,hr;q=0.9,en-US;q=0.8,en;q=0.7",
	"vinted.ie":    "en-IE,en;q=0.9",
	"vinted.co.uk": "en-GB,en;q=0.9",
}

// acceptLanguage picks the site's language from its host.
func (c *Client) acceptLanguage() string {
	host := strings.TrimPrefix(strings.ToLower(c.Host()), "www.")
	if l, ok := siteLanguages[host]; ok {
		return l
	}
	return "en-US,en;q=0.9"
}

// browserHeaders sets the headers Chrome sends for a page navigation (html)
// or for the web app's own fetch to the API. Accept-Encoding is left to the
// transport, which then decompresses transparently.
func (c *Client) browserHeaders(h http.Header, method string, html, gateway bool) {
	h.Set("User-Agent", browserUA)
	h.Set("Sec-Ch-Ua", browserCHUA)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", browserPlatform)
	h.Set("Accept-Language", c.acceptLanguage())
	if html {
		h.Set("Accept", navigateAccept)
		h.Set("Upgrade-Insecure-Requests", "1")
		h.Set("Sec-Fetch-Dest", "document")
		h.Set("Sec-Fetch-Mode", "navigate")
		h.Set("Sec-Fetch-Site", "none")
		h.Set("Sec-Fetch-User", "?1")
		return
	}
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	site := "same-origin"
	if gateway {
		site = "same-site"
	}
	h.Set("Sec-Fetch-Site", site)
	h.Set("Referer", c.Base+"/")
	if method != http.MethodGet && method != http.MethodHead {
		h.Set("Origin", c.Base)
	}
}
