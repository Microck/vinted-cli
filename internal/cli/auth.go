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
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/throttle"
	"github.com/Microck/vinted-cli/internal/vinted"
)

func (a *App) authCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Log in, inspect or remove the session for a profile"}
	cmd.AddCommand(a.authLoginCmd(), a.authStatusCmd(), a.authRefreshCmd(), a.authLogoutCmd())
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
  2. Export cookies as Netscape/text or JSON and save the file, or copy the
     browser's Cookie request header.
  3. Run this command and paste the file path or the header when asked, or
     pass --cookies FILE / --cookies-stdin.

The whole jar is kept in credentials.toml (mode 0600), because Vinted's
anti-bot cookies travel with the tokens. The access token is renewed through
Vinted's refresh endpoint when it is about to expire, and the renewed cookies
are written back. The site comes from the cookies' domain unless --domain is
given. Google, Apple and Facebook accounts work the same way.

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
			cookies, host, err := vinted.ParseCookieExport(src)
			if err != nil {
				return output.Usagef("%v", err)
			}
			if domain == "" {
				domain = host
			}
			domain = normalizeDomain(domain)
			if domain != "" && os.Getenv("VINTED_BASE_URL") == "" {
				a.useSite("https://" + domain)
			}
			// Validate by reading the account. A refresh during validation
			// updates the session in memory; it is saved below in one write.
			a.Session = vinted.NewSession(a.Client, cookies)
			a.Client.Session = a.Session
			me, err := a.Client.Me(cmd.Context())
			if err != nil {
				return err
			}
			a.Creds.Profiles[a.Profile] = config.Session{
				Cookies: a.Session.Cookies, UserID: me.ID, Login: me.Login,
				SessionExpires: a.Session.Expires, UpdatedAt: time.Now().UTC(),
			}
			if err := a.saveCreds(); err != nil {
				return err
			}
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
				return err
			}
			return a.Printer.Print(a.currentStatus())
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

// normalizeDomain turns a cookie domain (".vinted.fr") or a bare site
// ("vinted.fr") into the host the web app answers on ("www.vinted.fr").
func normalizeDomain(d string) string {
	d = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(d), "https://"), "http://")
	d = strings.Trim(strings.TrimPrefix(d, "."), "/")
	if d == "" {
		return ""
	}
	if strings.HasPrefix(d, "vinted.") {
		d = "www." + d
	}
	return d
}

type authStatus struct {
	Profile        string     `json:"profile"`
	LoggedIn       bool       `json:"logged_in"`
	Account        string     `json:"account,omitempty"`
	UserID         int64      `json:"user_id,omitempty"`
	Source         string     `json:"source,omitempty"`
	Domain         string     `json:"domain,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	SessionExpires *time.Time `json:"session_expires,omitempty"`
	Valid          *bool      `json:"session_valid,omitempty"`
	// Cooldown is set while the site is paused after a block.
	Cooldown *throttle.Status `json:"cooldown,omitempty"`
}

func (s authStatus) Pretty(w io.Writer, color bool) {
	rows := [][]string{{"profile", s.Profile}}
	if !s.LoggedIn {
		rows = append(rows, []string{"session", "none (run `vinted auth login`)"})
	} else {
		rows = append(rows, []string{"account", strings.TrimSpace(s.Account + " " + output.Dim(idString(s.UserID), color))}, []string{"source", s.Source})
		if s.UpdatedAt != nil {
			rows = append(rows, []string{"updated", s.UpdatedAt.Local().Format(time.RFC3339)})
		}
		if s.SessionExpires != nil {
			rows = append(rows, []string{"expires", s.SessionExpires.Local().Format(time.RFC3339)})
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
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Check first: a refresh it triggers updates the stored expiry. A
			// cooldown is not a verdict on the session, so it fails the check.
			var valid *bool
			if check && a.Session != nil {
				_, err := a.Client.Me(cmd.Context())
				var ve *vinted.Error
				if errors.As(err, &ve) && ve.Kind == vinted.KindBlocked {
					return err
				}
				valid = new(bool)
				*valid = err == nil
			}
			st := a.currentStatus()
			st.Valid = valid
			return a.Printer.Print(st)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "call Vinted to verify the session still works")
	return cmd
}

// currentStatus describes the active profile's session as stored right now.
func (a *App) currentStatus() authStatus {
	st := authStatus{Profile: a.Profile, Domain: a.Cfg.Profiles[a.Profile].Domain}
	if cd, err := a.Gate.Status(); err == nil && cd.Blocked {
		st.Cooldown = &cd
	}
	if os.Getenv("VINTED_COOKIES") != "" && a.Session != nil {
		// Env sessions are never persisted, so the only expiry known is the
		// one seen by this process.
		st.LoggedIn, st.Source = true, "env VINTED_COOKIES"
		st.UserID, st.SessionExpires = a.Session.UserID(), timePtr(a.Session.Expires)
	} else if s, ok := a.Creds.Profiles[a.Profile]; ok {
		st.LoggedIn, st.Source, st.Account, st.UserID = true, "credentials", s.Login, s.UserID
		st.UpdatedAt, st.SessionExpires = timePtr(s.UpdatedAt), timePtr(s.SessionExpires)
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
		Long: `Renew the access token now through Vinted's refresh endpoint and store the
renewed cookies. Commands refresh on their own shortly before expiry; this is
for checking that the refresh token still works. It fails when Vinted has
revoked the login, and then only a new "vinted auth login" helps.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			if err := a.Session.Refresh(cmd.Context()); err != nil {
				return err
			}
			return a.Printer.Print(a.currentStatus())
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
	Name           string     `json:"name"`
	Account        string     `json:"account,omitempty"`
	UserID         int64      `json:"user_id,omitempty"`
	Default        bool       `json:"default"`
	UpdatedAt      time.Time  `json:"updated_at,omitzero"`
	SessionExpires *time.Time `json:"session_expires,omitempty"`
}

type profileList []profileRow

func (l profileList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, p := range l {
		mark := ""
		if p.Default {
			mark = "*"
		}
		expires := ""
		if p.SessionExpires != nil {
			expires = p.SessionExpires.Local().Format(time.RFC3339)
		}
		rows = append(rows, []string{mark, p.Name, p.Account, output.Dim(idString(p.UserID), color), expires})
	}
	output.Table(w, color, []string{"", "PROFILE", "ACCOUNT", "USER ID", "EXPIRES"}, rows)
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
					list = append(list, profileRow{Name: name, Account: s.Login, UserID: s.UserID, Default: name == def, UpdatedAt: s.UpdatedAt, SessionExpires: timePtr(s.SessionExpires)})
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
