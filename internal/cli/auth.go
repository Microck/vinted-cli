package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/galleton"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/throttle"
	"github.com/Microck/vinted-cli/internal/vinted"
)

func (a *App) authCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Log in, inspect or remove the session for a profile"}
	cmd.AddCommand(a.authLoginCmd(), a.authStatusCmd(), a.authRefreshCmd(), a.authLogoutCmd(), a.authServiceCmd())
	return cmd
}

func (a *App) authLoginCmd() *cobra.Command {
	var cookiesFile, domain string
	var cookiesStdin bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Import a browser session for the active profile",
		Long: `Import a browser session for the active profile.

Vinted keeps the login in two browser cookies, access_token_web (short-lived)
and refresh_token_web (HttpOnly), so the CLI needs a cookie export from a
browser where you are logged in:

  1. Install a cookie export extension (Cookie-Editor works) and open your
     Vinted site (www.vinted.com, www.vinted.fr, www.vinted.es, ...).
  2. Export cookies as Netscape cookies.txt or JSON (Cookie-Editor,
     EditThisCookie, DevTools and Playwright lists all work) and save the
     file, or copy the browser's Cookie request header. A whole-browser
     export is fine: only the Vinted site's cookies are kept.
  3. Run this command and paste the file path or the header when asked, or
     pass --cookies FILE / --cookies-stdin.

The cookies go into the bundled Galleton session engine's encrypted vault,
never into credentials.toml. The engine renews the access token through
Vinted's refresh endpoint before it expires, keeps the rotated cookies, and
is the only party that ever uses the single-use refresh token. After the
import, stop using that browser login (log out there and log in again if
you need the website): the browser still holds the refresh token the engine
spends. The site comes from the cookies' domain unless --domain is given.
Google, Apple and Facebook accounts work the same way.

Examples:
  vinted auth login
  vinted auth login --cookies ~/Downloads/www.vinted.fr_cookies.txt --profile fr
  cat cookies.txt | vinted auth login --cookies-stdin --domain www.vinted.es`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cookiesFile != "" && cookiesStdin {
				return output.Usagef("--cookies and --cookies-stdin are exclusive; pass one")
			}
			var src io.Reader
			switch {
			case cookiesFile != "":
				f, err := openCookieFile(cookiesFile)
				if err != nil {
					return err
				}
				defer f.Close()
				src = f
			case cookiesStdin:
				src = a.Stdin
			case a.Interactive:
				fmt.Fprint(a.Stderr, "Paste the cookie export file path, or the Cookie header: ")
				line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
				line = strings.TrimSpace(line)
				if line == "" {
					return output.Usagef("nothing entered")
				}
				// A Cookie header carries name=value pairs and a JSON export
				// starts with a bracket; anything else is a path, and a path
				// that does not open is reported as such instead of being
				// parsed as a header with no tokens in it.
				if strings.Contains(line, "=") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "{") {
					src = strings.NewReader(line)
				} else {
					f, err := openCookieFile(line)
					if err != nil {
						return err
					}
					defer f.Close()
					src = f
				}
			default:
				return output.Usagef("no cookies given. Pass --cookies FILE or --cookies-stdin (prompts need a terminal)")
			}
			export, err := vinted.ReadCookieExport(src, siteHostname(a.Client.Base))
			if err != nil {
				return output.Usagef("%v", err)
			}
			if domain == "" {
				if len(export.Sites) > 1 {
					return output.Usagef("the export holds logins for several vinted sites (%s); pass --domain with the one this profile is for", strings.Join(export.Sites, ", "))
				}
				domain = export.Site
			}
			domain = vinted.NormalizeSite(domain)
			if domain != "" && os.Getenv("VINTED_BASE_URL") == "" {
				a.useSite("https://" + domain)
			}
			return a.managedLogin(cmd.Context(), export, domain)
		},
	}
	cmd.Flags().StringVar(&cookiesFile, "cookies", "", "path to a cookie export (Netscape, JSON, or a Cookie header)")
	cmd.Flags().BoolVar(&cookiesStdin, "cookies-stdin", false, "read the cookie export from stdin")
	cmd.Flags().StringVar(&domain, "domain", "", "Vinted site, e.g. www.vinted.fr (default: the cookies' domain)")
	return cmd
}

// openCookieFile opens a cookie export, expanding a leading ~/ the way a
// shell would have: a path pasted at the prompt never went through one. A
// file that cannot be read is the caller's mistake, so a usage error.
func openCookieFile(path string) (*os.File, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		path = filepath.Join(config.UserHome(), rest)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, output.Usagef("cannot read the cookie export: %v", err)
	}
	if st, err := f.Stat(); err == nil && st.IsDir() {
		f.Close()
		return nil, output.Usagef("%s is a directory, not a cookie export", path)
	}
	return f, nil
}

type authStatus struct {
	Profile   string     `json:"profile"`
	LoggedIn  bool       `json:"logged_in"`
	Account   string     `json:"account,omitempty"`
	UserID    int64      `json:"user_id,omitempty"`
	Source    string     `json:"source,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	Domain    string     `json:"domain,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	// Legacy is set for a profile an older vinted-cli saved; it needs a new login.
	Legacy bool `json:"needs_login,omitempty"`
	// Engine is the session engine's view, read only by --check and refresh.
	Engine *engineState `json:"engine,omitempty"`
	Valid  *bool        `json:"session_valid,omitempty"`
	// Cooldown is set while the site is paused after a block.
	Cooldown *throttle.Status `json:"cooldown,omitempty"`
}

// engineState is the non-secret session metadata the Galleton daemon keeps.
type engineState struct {
	Status        string     `json:"status"`
	LastRefresh   *time.Time `json:"last_refresh,omitempty"`
	NextRefresh   *time.Time `json:"next_refresh,omitempty"`
	AccessExpires *time.Time `json:"access_expires,omitempty"`
	Cookies       int        `json:"cookies"`
}

func engineStateOf(m galleton.Metadata) *engineState {
	return &engineState{Status: m.Status, LastRefresh: timePtr(m.LastRefresh), NextRefresh: timePtr(m.NextRefresh), AccessExpires: timePtr(m.AccessExpiresAt), Cookies: m.CookieCount}
}

func (s authStatus) Pretty(w io.Writer, color bool) {
	rows := [][]string{{"profile", s.Profile}}
	switch {
	case s.Legacy:
		rows = append(rows, []string{"session", "saved by an older vinted-cli; run `vinted auth login` again"})
	case !s.LoggedIn:
		rows = append(rows, []string{"session", "none (run `vinted auth login`)"})
	default:
		rows = append(rows, []string{"account", strings.TrimSpace(s.Account + " " + output.Dim(idString(s.UserID), color))}, []string{"source", s.Source})
		if s.UpdatedAt != nil {
			rows = append(rows, []string{"logged in", s.UpdatedAt.Local().Format(time.RFC3339)})
		}
		if e := s.Engine; e != nil {
			rows = append(rows, []string{"engine", e.Status})
			if e.LastRefresh != nil {
				rows = append(rows, []string{"renewed", e.LastRefresh.Local().Format(time.RFC3339)})
			}
			if e.AccessExpires != nil {
				rows = append(rows, []string{"expires", e.AccessExpires.Local().Format(time.RFC3339)})
			}
			if e.NextRefresh != nil {
				rows = append(rows, []string{"next renewal", e.NextRefresh.Local().Format(time.RFC3339)})
			}
		}
		if s.Valid != nil {
			rows = append(rows, []string{"valid", fmt.Sprint(*s.Valid)})
		}
	}
	if s.Domain != "" {
		rows = append(rows, []string{"site", s.Domain})
	}
	if c := s.Cooldown; c != nil {
		rows = append(rows, []string{"cooldown", "blocked by vinted; no requests until " + c.BlockedUntil.Local().Format("2006-01-02 15:04 MST") + " (" + c.Remaining + " left), see `vinted cooldown`"})
	}
	output.Table(w, color, nil, rows)
}

// idString renders a numeric id, or nothing when it is unknown.
func idString(id int64) string {
	if id == 0 {
		return ""
	}
	return fmt.Sprint(id)
}

func (a *App) authStatusCmd() *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the active profile's session",
		Long: `Show the active profile's session as stored locally, without contacting
anything. --check also asks the session engine for its state and reads the
account from Vinted, which renews the access token first when it is due.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st := a.currentStatus()
			if !check || a.Account == nil {
				return a.Printer.Print(st)
			}
			// A cooldown is not a verdict on the session, so it fails the check.
			_, err := a.Client.Me(cmd.Context())
			var ve *vinted.Error
			if errors.As(err, &ve) && ve.Kind == vinted.KindBlocked {
				return err
			}
			valid := err == nil
			st.Valid = &valid
			if meta, err := a.Account.status(cmd.Context()); err == nil {
				st.Engine = engineStateOf(meta)
			}
			return a.Printer.Print(st)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "ask the session engine and Vinted whether the session still works")
	return cmd
}

// currentStatus describes the active profile's session as stored right now.
func (a *App) currentStatus() authStatus {
	st := authStatus{Profile: a.Profile, Domain: a.Cfg.Profiles[a.Profile].Domain}
	if cd, err := a.Gate.Status(); err == nil && cd.Blocked {
		st.Cooldown = &cd
	}
	if s, ok := a.Creds.Profiles[a.Profile]; ok {
		if s.GalletonID == "" {
			st.Legacy = true
			return st
		}
		st.LoggedIn, st.Source, st.SessionID, st.Account, st.UserID = true, "galleton", s.GalletonID, s.Login, s.UserID
		st.UpdatedAt = timePtr(s.UpdatedAt)
	}
	return st
}

// timePtr turns a zero time into nil so JSON omits unknown timestamps.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *App) authRefreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Renew the access token now",
		Long: `Ask the session engine to renew the access token now through Vinted's
refresh endpoint. It renews on its own before expiry; this is for checking
that the refresh token still works. It fails when Vinted has revoked the
login, and then only a new "vinted auth login" helps.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			// The renewal is a request to Vinted like any other: it waits for
			// the gate, so a cooldown or the pacing applies to it too.
			if err := a.Gate.Wait(cmd.Context()); err != nil {
				return err
			}
			if err := a.Account.Renew(cmd.Context()); err != nil {
				return err
			}
			st := a.currentStatus()
			if meta, err := a.Account.status(cmd.Context()); err == nil {
				st.Engine = engineStateOf(meta)
			}
			return a.Printer.Print(st)
		},
	}
}

func (a *App) authLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Forget the active profile's session (keeps watches and config)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, ok := a.Creds.Profiles[a.Profile]; !ok {
				return a.Printer.Print(authStatus{Profile: a.Profile})
			}
			// Forget the engine's copy first; a failure keeps the reference
			// for a retry. This does not log the browser out.
			if err := a.forgetManagedProfile(cmd.Context(), a.Profile); err != nil {
				return err
			}
			delete(a.Creds.Profiles, a.Profile)
			if err := a.saveCreds(); err != nil {
				return err
			}
			return a.Printer.Print(authStatus{Profile: a.Profile})
		},
	}
	cmd.Flags().Bool("yes", false, "accepted for symmetry; logout never prompts")
	return cmd
}

// profile

type profileRow struct {
	Name      string    `json:"name"`
	Account   string    `json:"account,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	Default   bool      `json:"default"`
	SessionID string    `json:"session_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	// NeedsLogin marks a profile an older vinted-cli saved.
	NeedsLogin bool `json:"needs_login,omitempty"`
}

type profileList []profileRow

func (l profileList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, p := range l {
		mark := ""
		if p.Default {
			mark = "*"
		}
		login := ""
		if !p.UpdatedAt.IsZero() {
			login = p.UpdatedAt.Local().Format(time.RFC3339)
		}
		if p.NeedsLogin {
			login = "run vinted auth login"
		}
		rows = append(rows, []string{mark, p.Name, p.Account, output.Dim(idString(p.UserID), color), login})
	}
	output.Table(w, color, []string{"", "PROFILE", "ACCOUNT", "USER ID", "LOGGED IN"}, rows)
}

func (a *App) profileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "Manage named accounts"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List profiles with a session", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				def := config.ResolveProfile("", a.Cfg)
				list := profileList{}
				for name, s := range a.Creds.Profiles {
					list = append(list, profileRow{Name: name, Account: s.Login, UserID: s.UserID, Default: name == def, SessionID: s.GalletonID, UpdatedAt: s.UpdatedAt, NeedsLogin: s.GalletonID == ""})
				}
				sortProfiles(list)
				return a.Printer.Print(list)
			},
		},
		&cobra.Command{
			Use: "use NAME", Short: "Make a profile the default", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, ok := a.Creds.Profiles[args[0]]; !ok {
					return output.Usagef("profile %q has no session. Run `vinted auth login --profile %s` first", args[0], args[0])
				}
				a.Cfg.DefaultProfile = args[0]
				if err := a.saveConfig(); err != nil {
					return err
				}
				return a.Printer.Print(map[string]string{"default_profile": args[0]})
			},
		},
		a.profileRemoveCmd(),
	)
	return cmd
}

func (a *App) profileRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: "remove NAME", Short: "Delete a profile's session, config and watches", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := a.confirm(yes, fmt.Sprintf("Remove profile %q, its session and its watches?", name)); err != nil {
				return err
			}
			if err := a.forgetManagedProfile(cmd.Context(), name); err != nil {
				return err
			}
			delete(a.Creds.Profiles, name)
			if err := a.saveCreds(); err != nil {
				return err
			}
			delete(a.Cfg.Profiles, name)
			if a.Cfg.DefaultProfile == name {
				a.Cfg.DefaultProfile = ""
			}
			if err := a.saveConfig(); err != nil {
				return err
			}
			st, err := a.Store()
			if err != nil {
				return err
			}
			if err := st.RemoveProfileWatches(cmd.Context(), name); err != nil {
				return err
			}
			return a.Printer.Print(map[string]string{"removed": name})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not prompt")
	return cmd
}

func sortProfiles(l profileList) {
	for i := 1; i < len(l); i++ {
		for j := i; j > 0 && l[j].Name < l[j-1].Name; j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}
