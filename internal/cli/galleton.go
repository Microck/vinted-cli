package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Microck/galleton/cookiefile"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/galleton"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// managedAccount is the logged-in profile as a vinted.Account. Every request
// carrying the account's cookies goes through the Galleton daemon: it
// attaches the cookies, renews them through Vinted's refresh endpoint, and
// keeps whatever each response sets. After login the CLI never holds a cookie, so two
// processes can never spend the same single-use refresh token.
type managedAccount struct {
	mu       sync.Mutex
	app      *App
	id       string
	provider string
	userID   int64
	checked  bool
}

func (m *managedAccount) client(ctx context.Context) (*galleton.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, err := m.app.galletonClient(ctx)
	if err != nil {
		return nil, managedError(err)
	}
	if m.checked {
		return c, nil
	}
	meta, err := c.Status(ctx, m.id)
	if err != nil {
		return nil, managedError(err)
	}
	if meta.ID != m.id || meta.Provider != m.provider {
		return nil, &vinted.Error{Kind: vinted.KindAuth, Msg: fmt.Sprintf("the profile's session belongs to another Vinted site (%s, not %s). Run `vinted auth login` for this site", meta.Provider, m.provider)}
	}
	m.checked = true
	if s := m.app.Creds.Profiles[m.app.Profile]; len(s.Replaced) > 0 {
		m.app.dropReplaced(ctx, c, s.Replaced)
	}
	return c, nil
}

func (m *managedAccount) Send(ctx context.Context, method, rawURL string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	c, err := m.client(ctx)
	if err != nil {
		return 0, nil, nil, err
	}
	flat := make(map[string]string, len(header))
	for k, v := range header {
		if len(v) > 0 {
			flat[k] = v[0]
		}
	}
	resp, err := c.Request(ctx, m.id, method, rawURL, flat, body)
	if err != nil {
		return 0, nil, nil, m.engineError(ctx, err)
	}
	return resp.Status, resp.Headers, resp.Body, nil
}

func (m *managedAccount) Renew(ctx context.Context) error {
	c, err := m.client(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Refresh(ctx, m.id); err != nil {
		return m.engineError(ctx, err)
	}
	return nil
}

// engineError is managedError for request and renewal failures. When the
// bot protection blocked the engine's own renewal, the CLI starts the same
// shared cooldown (and engine pause) as for a block it saw itself, so no
// process keeps knocking while the site refuses.
func (m *managedAccount) engineError(ctx context.Context, err error) error {
	var apiErr *galleton.Error
	if !errors.As(err, &apiErr) || apiErr.Code != "upstream_blocked" {
		return managedError(err)
	}
	what := "vinted's bot protection (Datadome/Cloudflare) blocked the session engine's token renewal"
	e := &vinted.Error{Kind: vinted.KindBlocked, Status: apiErr.Status, Endpoint: "POST " + vinted.RefreshPath}
	retry := time.Duration(0)
	if !apiErr.RetryAt.IsZero() {
		retry = time.Until(apiErr.RetryAt)
	}
	wait, berr := m.app.Gate.Block(what, retry)
	e.Msg = what + ". Stop running vinted commands: the cli will not contact vinted for the next " + vinted.FormatWait(wait) + ". Open vinted in your normal browser; if it shows a captcha or a block page, solve it there"
	if berr != nil {
		e.Msg += fmt.Sprintf(" (the cooldown could not be saved: %v)", berr)
	}
	if wait > 0 {
		if perr := m.app.pauseSessions(ctx, wait); perr != nil {
			e.Msg += fmt.Sprintf(" (the session engine could not be paused: %v)", perr)
		}
	}
	e.Suggest = vinted.BlockAdvice(m.app.Client.Base, wait)
	return e
}

// sessionsClient is the engine to pause or resume for the site: this
// invocation's lease when it has one or a logged-in profile, else an engine
// that is already running (none is started just for this). nil when none.
func (a *App) sessionsClient(ctx context.Context) (*galleton.Client, error) {
	if a.sessionClient != nil {
		return a.sessionClient, nil
	}
	if a.Account != nil {
		c, err := a.galletonClient(ctx)
		var held *engineHeldError
		if errors.As(err, &held) {
			// Not running and kept stopped by a cooldown: nothing to pause or resume.
			return nil, nil
		}
		if err != nil {
			return nil, managedError(err)
		}
		return c, nil
	}
	if galleton.Configured() {
		c, err := galleton.FromEnv()
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	return galleton.IfRunning(ctx, a.sessionDir())
}

// pauseSessions pauses the engine's provider for this site after a block,
// for every profile. An engine started later sees the cooldown in
// galletonClient; without any engine there is nothing to pause.
func (a *App) pauseSessions(ctx context.Context, d time.Duration) error {
	c, err := a.sessionsClient(ctx)
	if err != nil || c == nil {
		return err
	}
	if err := c.Pause(ctx, vinted.Provider(a.Client.Base), d); err != nil {
		return managedError(err)
	}
	return nil
}

// resumeSessions lifts the engine's pause on this site, for every profile.
func (a *App) resumeSessions(ctx context.Context) error {
	c, err := a.sessionsClient(ctx)
	if err != nil || c == nil {
		return err
	}
	if err := c.Resume(ctx, vinted.Provider(a.Client.Base)); err != nil {
		return managedError(err)
	}
	return nil
}

func (m *managedAccount) UserID() int64 { return m.userID }

// status reads the daemon's view of the session without renewing it.
func (m *managedAccount) status(ctx context.Context) (galleton.Metadata, error) {
	c, err := m.client(ctx)
	if err != nil {
		return galleton.Metadata{}, err
	}
	meta, err := c.Status(ctx, m.id)
	if err != nil {
		return meta, managedError(err)
	}
	return meta, nil
}

// managedError maps a daemon failure onto the CLI's error kinds. Nothing is
// replayed automatically: an import, renewal or request may have completed.
func managedError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ve *vinted.Error
	if errors.As(err, &ve) {
		return err
	}
	e := &vinted.Error{Kind: vinted.KindNetwork, Msg: err.Error()}
	var apiErr *galleton.Error
	if !errors.As(err, &apiErr) {
		return e
	}
	e.Status = apiErr.Status
	until := ""
	if !apiErr.RetryAt.IsZero() {
		until = " until " + apiErr.RetryAt.Local().Format("2006-01-02 15:04 MST")
	}
	switch {
	case apiErr.Code == "reauth_required" || apiErr.Code == "renewal_uncertain" || apiErr.Code == "protocol_error" || apiErr.Status == http.StatusNotFound:
		e.Kind = vinted.KindAuth
		e.Msg = "vinted rejected the stored session or a renewal could not be confirmed. Log in on the website, export the cookies again and run `vinted auth login`"
	case apiErr.Code == "provider_paused":
		e.Kind = vinted.KindBlocked
		e.Msg = "the session engine is paused after a block by vinted's bot protection" + until + "; nothing is sent meanwhile. See `vinted cooldown`"
		e.Suggest = []string{"vinted cooldown"}
	case apiErr.Code == "upstream_blocked" || apiErr.Code == "retry_later" || apiErr.Status == http.StatusTooManyRequests:
		e.Kind = vinted.KindBlocked
		e.Msg = "the session engine is waiting before it contacts vinted again" + until + ". Stop running vinted commands meanwhile"
	case apiErr.Code == "configuration_error":
		e.Kind = vinted.KindAuth
		e.Msg = "the session was imported under a different session engine configuration for this site. Run `vinted auth login` again"
	case apiErr.Code == "upstream_request_failed":
		e.Msg = "the request reached vinted, or may have, but no answer came back. It was not retried, because it may have taken effect; check before running it again"
	case apiErr.Code == "body_too_large" || apiErr.Code == "response_too_large":
		e.Kind = vinted.KindGeneric
		e.Msg = "the request or vinted's answer exceeds the session engine's size limit"
	case apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden:
		e.Kind = vinted.KindAuth
		e.Msg = "the session engine rejected local access; check its api.token and the site's allowed origins"
	case apiErr.Status < 500:
		e.Kind = vinted.KindGeneric
		e.Msg = "the session engine rejected the request (" + apiErr.Code + ")"
	default:
		e.Msg = "the session engine could not complete the operation (" + apiErr.Code + "); inspect it with `vinted auth service status` before retrying"
	}
	return e
}

// newSessionID names one login's daemon session: namespaced by credentials
// file and profile, so separate CLI homes sharing an external daemon never
// collide and arbitrary profile names stay valid IDs, plus a random part, so
// a new login never overwrites the session it is meant to replace.
func (a *App) newSessionID(profile string) (string, error) {
	path, err := filepath.Abs(a.Paths.CredentialsFile)
	if err != nil {
		path = filepath.Clean(a.Paths.CredentialsFile)
	}
	sum := sha256.Sum256([]byte(path + "\x00" + profile))
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("vinted-%x-%x", sum[:20], nonce), nil
}

// sessionDir holds the bundled daemon's encrypted vault, next to credentials.toml.
func (a *App) sessionDir() string {
	return filepath.Join(filepath.Dir(a.Paths.CredentialsFile), "galleton")
}

// engineSetup is the daemon configuration this invocation needs: the
// adapter for every Vinted site (and VINTED_BASE_URL's), and the proxy all
// renewals and account requests must leave through.
func (a *App) engineSetup() (galleton.Setup, error) {
	if a.proxyErr != nil {
		return galleton.Setup{}, a.proxyErr
	}
	// A bad request_interval stops every request; the engine's renewals too.
	if err := a.Gate.Err(); err != nil {
		return galleton.Setup{}, err
	}
	proxy := a.proxyURL
	if proxy == "" {
		// Without a configured proxy the guest requests follow HTTPS_PROXY
		// and friends (Go's default transport); the engine ignores its
		// environment and has one route for every site it serves, so hand it
		// the proxy the environment picks for all of their origins, and
		// refuse when NO_PROXY splits them.
		var err error
		if proxy, err = envProxy(vinted.AllOrigins(a.Client.Base)); err != nil {
			return galleton.Setup{}, err
		}
	}
	env := ""
	if proxy != "" {
		env = galleton.ProxyEnv
	}
	adapter, err := vinted.Adapter(a.Client.Base, env, a.Gate.Interval())
	if err != nil {
		return galleton.Setup{}, err
	}
	return galleton.Setup{Adapter: adapter, Proxy: proxy}, nil
}

func (a *App) galletonClient(ctx context.Context) (*galleton.Client, error) {
	if a.sessionClient != nil {
		return a.sessionClient, nil
	}
	if galleton.Configured() {
		c, err := galleton.FromEnv()
		if err != nil {
			return nil, err
		}
		if err := a.carryCooldown(ctx, c); err != nil {
			c.Close()
			return nil, err
		}
		a.sessionClient = c
		return c, nil
	}
	c, err := a.openEngine(ctx, a.sessionDir())
	if err != nil {
		return nil, err
	}
	a.sessionClient = c
	return c, nil
}

// openEngine attaches to the engine in dir, starting it if needed, and
// pauses it for any running cooldown. The engine renews due sessions the
// moment it starts, before a pause can reach it, so during a cooldown it is
// only attached to, never started.
func (a *App) openEngine(ctx context.Context, dir string) (*galleton.Client, error) {
	setup, err := a.engineSetup()
	if err != nil {
		return nil, err
	}
	// Checked at the last moment before a start, so a block recorded by
	// another process while this one was getting ready still holds it.
	setup.MayLaunch = func() error {
		pauses, err := a.enginePauses()
		if err != nil {
			return err
		}
		if len(pauses) > 0 {
			return &engineHeldError{&vinted.Error{Kind: vinted.KindBlocked, Msg: "a cooldown is running, so the session engine stays stopped until it ends: once started it would renew sessions against the blocked site before it could be paused. Check `vinted cooldown status`"}}
		}
		return nil
	}
	c, err := galleton.Open(ctx, dir, setup)
	if err != nil {
		return nil, err
	}
	if err := a.carryCooldown(ctx, c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// engineHeldError is openEngine refusing to start the engine during a
// cooldown.
type engineHeldError struct{ err *vinted.Error }

func (e *engineHeldError) Error() string { return e.err.Error() }
func (e *engineHeldError) Unwrap() error { return e.err }

// enginePauses is how long each engine provider must stay paused for the
// cooldowns that are running.
func (a *App) enginePauses() (map[string]time.Duration, error) {
	// Every site's cooldown, not only this profile's: the engine renews the
	// sessions of all profiles.
	blocked, err := a.Gate.Cooldowns()
	if err != nil {
		return nil, err
	}
	current := ""
	if u, err := url.Parse(a.Client.Base); err == nil {
		current = u.Host
	}
	pauses := map[string]time.Duration{}
	for host, until := range blocked {
		base := "https://" + host
		if host == current {
			base = a.Client.Base
		}
		provider := vinted.Provider(base)
		// Every custom host maps to one provider name, but the engine is set up
		// for this profile's custom site only: another one's cooldown is not ours.
		if provider == "vinted-custom" && host != current {
			continue
		}
		if d := time.Until(until); d > pauses[provider] {
			pauses[provider] = d
		}
	}
	return pauses, nil
}

// carryCooldown pauses an attached engine for the rest of any running
// cooldown, in case the block was seen while it did not watch. It fails
// closed: an unreadable cooldown or a failed pause keeps the engine unused.
func (a *App) carryCooldown(ctx context.Context, c *galleton.Client) error {
	pauses, err := a.enginePauses()
	if err != nil {
		return err
	}
	for provider, d := range pauses {
		if err := c.Pause(ctx, provider, d); err != nil {
			return fmt.Errorf("could not pause the session engine for the cooldown on %s: %w", provider, managedError(err))
		}
	}
	return nil
}

// forgetManagedProfile deletes a profile's daemon session before its local
// reference goes. A failed deletion keeps the reference for a retry. It does
// not log the browser out.
func (a *App) forgetManagedProfile(ctx context.Context, profile string) error {
	s, ok := a.Creds.Profiles[profile]
	if !ok || s.GalletonID == "" {
		return nil
	}
	c, err := a.galletonClient(ctx)
	if err != nil {
		return managedError(err)
	}
	for _, id := range append(append([]string{}, s.Replaced...), s.GalletonID) {
		if err := c.Forget(ctx, id); err != nil {
			return managedError(err)
		}
	}
	return nil
}

// dropReplaced deletes engine sessions a login replaced, and records in
// credentials.toml the ones that still could not be deleted.
func (a *App) dropReplaced(ctx context.Context, c *galleton.Client, ids []string) {
	var left []string
	for _, id := range ids {
		if err := c.Forget(ctx, id); err != nil {
			fmt.Fprintf(a.Stderr, "vinted: could not remove the replaced session %s from the engine yet (it is retried later): %v\n", id, managedError(err))
			left = append(left, id)
		}
	}
	s, ok := a.Creds.Profiles[a.Profile]
	if !ok || slices.Equal(s.Replaced, left) {
		return
	}
	s.Replaced = left
	a.Creds.Profiles[a.Profile] = s
	if err := a.saveCreds(); err != nil {
		fmt.Fprintf(a.Stderr, "vinted: could not record the sessions left to remove: %v\n", err)
	}
}

// envProxy is the proxy the standard environment variables pick for origins,
// empty for a direct connection. Every origin must get the same answer.
func envProxy(origins []string) (string, error) {
	choice := ""
	for i, o := range origins {
		u, err := url.Parse(o)
		if err != nil {
			return "", err
		}
		p, err := http.ProxyFromEnvironment(&http.Request{URL: u})
		if err != nil {
			return "", output.Usagef("HTTPS_PROXY/HTTP_PROXY: %v", err)
		}
		got := ""
		if p != nil {
			got = p.String()
		}
		if i > 0 && got != choice {
			return "", output.Usagef("NO_PROXY sends %s and %s different ways, but the session engine has one route for every Vinted site. Set VINTED_PROXY, or make NO_PROXY cover all Vinted hosts or none", origins[0], o)
		}
		choice = got
	}
	return choice, nil
}

// managedLogin imports an export into the daemon under a new session ID,
// validates it by reading the account through the daemon, and only then
// points the profile at it and deletes the session it replaces. A login that
// fails leaves the previous session as it was.
func (a *App) managedLogin(ctx context.Context, export vinted.CookieExport, domain string) error {
	base := a.Client.Base
	cookies := export.For(base)
	// The export passes through this process once; keep its secrets out of
	// --debug output and error bodies (short values are flags, not secrets).
	for _, c := range export.Cookies {
		if len(c.Value) >= 16 {
			a.Client.Redact(c.Value)
		}
	}
	if !hasCookie(cookies, vinted.RefreshCookie) {
		return output.Usagef("the export has no %s for %s; export the cookies of a logged-in tab of that site, or pass --domain", vinted.RefreshCookie, siteHostname(base))
	}
	provider := vinted.Provider(base)
	id, err := a.newSessionID(a.Profile)
	if err != nil {
		return err
	}
	previous := a.Creds.Profiles[a.Profile].GalletonID
	pending := append([]string{}, a.Creds.Profiles[a.Profile].Replaced...)
	c, err := a.galletonClient(ctx)
	if err != nil {
		return managedError(err)
	}
	in := galleton.Credentials{Provider: provider, CookieOrigin: base, Cookies: cookies}
	// The expiry of this site's own access token: an export can carry
	// another site's login too.
	if exp := vinted.AccessExpiry(cookies); !exp.IsZero() {
		exp = exp.UTC()
		in.AccessExpiresAt = &exp
	}
	// The daemon renews an imported session right away. That renewal is a
	// request to Vinted like any other, so it waits for the gate (and a
	// cooldown refuses the login) before the import that triggers it.
	if err := a.Gate.Wait(ctx); err != nil {
		return err
	}
	forget := func(id, what string) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		err := c.Forget(cleanup, id)
		if err == nil {
			return
		}
		// Queue it with the profile's replaced sessions, which the next
		// command that reaches the engine deletes. A profile without a
		// session yet has nothing to hang it on: the warning names it.
		if s, ok := a.Creds.Profiles[a.Profile]; ok && s.GalletonID != "" && !slices.Contains(s.Replaced, id) {
			s.Replaced = append(s.Replaced, id)
			a.Creds.Profiles[a.Profile] = s
			if serr := a.saveCreds(); serr == nil {
				fmt.Fprintf(a.Stderr, "vinted: could not remove the %s session %s from the engine yet (it is retried later): %v\n", what, id, managedError(err))
				return
			}
			s.Replaced = s.Replaced[:len(s.Replaced)-1]
			a.Creds.Profiles[a.Profile] = s
		}
		fmt.Fprintf(a.Stderr, "vinted: could not remove the %s session %s from the engine: %v\n", what, id, managedError(err))
	}
	if _, err := c.Connect(ctx, id, in); err != nil {
		// A lost answer or a cancel may hide a completed import: remove the
		// new session either way (forgetting a missing one is a no-op).
		forget(id, "unconfirmed")
		return managedError(err)
	}
	a.Account = &managedAccount{app: a, id: id, provider: provider, checked: true}
	a.Client.Account = a.Account
	me, err := a.Client.Me(ctx)
	if err != nil {
		forget(id, "rejected")
		return err
	}
	a.Account.userID = me.ID
	// The profile's site goes to config.toml first: a session for one site
	// must never be recorded next to another site's domain. Each step that
	// fails undoes the ones before it and keeps the previous session.
	oldDefault := a.Cfg.DefaultProfile
	oldProfile, hadProfile := a.Cfg.Profiles[a.Profile]
	// First profile becomes the default; the site sticks to the profile.
	if a.Cfg.DefaultProfile == "" {
		a.Cfg.DefaultProfile = a.Profile
	}
	if a.Cfg.Profiles == nil {
		a.Cfg.Profiles = map[string]config.ProfileConfig{}
	}
	if domain != "" {
		p := a.Cfg.Profiles[a.Profile]
		p.Domain = domain
		a.Cfg.Profiles[a.Profile] = p
	}
	if err := a.saveConfig(); err != nil {
		forget(id, "new")
		return fmt.Errorf("config.toml could not be saved, so the profile keeps its previous session: %w", err)
	}
	// The sessions this login replaces are recorded in the same write, so a
	// crash before they are deleted still leaves them reachable for cleanup.
	if previous != "" && previous != id {
		pending = append(pending, previous)
	}
	a.Creds.Profiles[a.Profile] = config.Session{GalletonID: id, Replaced: pending, UserID: me.ID, Login: me.Login, UpdatedAt: time.Now().UTC()}
	if err := a.saveCreds(); err != nil {
		a.Cfg.DefaultProfile = oldDefault
		if hadProfile {
			a.Cfg.Profiles[a.Profile] = oldProfile
		} else {
			delete(a.Cfg.Profiles, a.Profile)
		}
		if cerr := a.saveConfig(); cerr != nil {
			fmt.Fprintf(a.Stderr, "vinted: could not restore config.toml: %v\n", cerr)
		}
		forget(id, "new")
		return fmt.Errorf("credentials.toml could not be saved, so the profile keeps its previous session: %w", err)
	}
	// Delete the replaced session (and any left from earlier logins). One
	// that cannot be deleted now stays recorded and is retried later, so no
	// unreferenced credential is left renewing in the vault.
	a.dropReplaced(ctx, c, pending)
	return a.Printer.Print(a.currentStatus())
}

func hasCookie(list []cookiefile.Cookie, name string) bool {
	for _, c := range list {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}

// siteHostname is a base URL's host name, without port.
func siteHostname(base string) string {
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return base
}
