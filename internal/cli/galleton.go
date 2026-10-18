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
// keeps whatever each response sets. The CLI never holds a cookie, so two
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
		return 0, nil, nil, managedError(err)
	}
	return resp.Status, resp.Headers, resp.Body, nil
}

func (m *managedAccount) Renew(ctx context.Context) error {
	c, err := m.client(ctx)
	if err != nil {
		return err
	}
	if _, err := c.Refresh(ctx, m.id); err != nil {
		return managedError(err)
	}
	return nil
}

func (m *managedAccount) Pause(ctx context.Context, d time.Duration) error {
	c, err := m.app.galletonClient(ctx)
	if err != nil {
		return managedError(err)
	}
	if err := c.Pause(ctx, m.provider, d); err != nil {
		return managedError(err)
	}
	return nil
}

func (m *managedAccount) UserID() int64 { return m.userID }

// resume lifts the engine's pause on the account's site.
func (m *managedAccount) resume(ctx context.Context) error {
	c, err := m.app.galletonClient(ctx)
	if err != nil {
		return managedError(err)
	}
	if err := c.Resume(ctx, m.provider); err != nil {
		return managedError(err)
	}
	return nil
}

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
	proxy := a.proxyURL
	if proxy == "" {
		// Without a configured proxy the guest requests follow HTTPS_PROXY
		// and friends (Go's default transport); the engine ignores its
		// environment, so hand it the same proxy explicitly.
		if u, err := url.Parse(a.Client.Base); err == nil {
			if p, err := http.ProxyFromEnvironment(&http.Request{URL: u}); err == nil && p != nil {
				proxy = p.String()
			}
		}
	}
	env := ""
	if proxy != "" {
		env = galleton.ProxyEnv
	}
	adapter, err := vinted.Adapter(a.Client.Base, env)
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
		a.sessionClient = c
		return c, nil
	}
	setup, err := a.engineSetup()
	if err != nil {
		return nil, err
	}
	c, err := galleton.Open(ctx, a.sessionDir(), setup)
	if err != nil {
		return nil, err
	}
	a.sessionClient = c
	return c, nil
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
	if err := c.Forget(ctx, s.GalletonID); err != nil {
		return managedError(err)
	}
	return nil
}

// managedLogin imports an export into the daemon under a new session ID,
// validates it by reading the account through the daemon, and only then
// points the profile at it and deletes the session it replaces. A login that
// fails leaves the previous session as it was.
func (a *App) managedLogin(ctx context.Context, export vinted.CookieExport, domain string) error {
	base := a.Client.Base
	cookies := export.For(base)
	if !hasCookie(cookies, vinted.RefreshCookie) {
		return output.Usagef("the export has no %s for %s; export the cookies of a logged-in tab of that site, or pass --domain", vinted.RefreshCookie, siteHostname(base))
	}
	provider := vinted.Provider(base)
	id, err := a.newSessionID(a.Profile)
	if err != nil {
		return err
	}
	previous := a.Creds.Profiles[a.Profile].GalletonID
	c, err := a.galletonClient(ctx)
	if err != nil {
		return managedError(err)
	}
	in := galleton.Credentials{Provider: provider, CookieOrigin: base, Cookies: cookies}
	if !export.AccessExpires.IsZero() {
		exp := export.AccessExpires.UTC()
		in.AccessExpiresAt = &exp
	}
	// The import goes to the local daemon, not to Vinted; the gate guards the
	// account read below and the renewal the daemon starts right away.
	if _, err := c.Connect(ctx, id, in); err != nil {
		return managedError(err)
	}
	forget := func(id, what string) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := c.Forget(cleanup, id); err != nil {
			fmt.Fprintf(a.Stderr, "vinted: could not remove the %s session %s from the engine: %v\n", what, id, managedError(err))
		}
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
	a.Creds.Profiles[a.Profile] = config.Session{GalletonID: id, UserID: me.ID, Login: me.Login, UpdatedAt: time.Now().UTC()}
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
	if previous != "" && previous != id {
		forget(previous, "replaced")
	}
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
