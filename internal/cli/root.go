// Package cli wires cobra commands to the vinted client, the store and the
// output layer. One file per noun. Command handlers stay thin: parse flags,
// call the client, print.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/store"
	"github.com/Microck/vinted-cli/internal/throttle"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// App is the per-invocation state shared by every command.
type App struct {
	loadedCreds config.Credentials

	Version string
	Paths   config.Paths
	Cfg     config.Config
	Creds   config.Credentials
	Profile string
	Client  *vinted.Client
	Session *vinted.Session
	// Gate is the shared cooldown, pacing and budget for the client's site.
	Gate *throttle.Gate
	// Proxy is the proxy all Vinted traffic goes through, without
	// credentials; empty for a direct connection.
	Proxy   string
	Printer output.Printer

	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	// Interactive is true only when stdin, stdout and stderr are all TTYs and
	// --no-input is absent. Prompts happen only then.
	Interactive bool

	flagProfile     string
	flagFormat      string
	flagNoColor     bool
	flagErrorFormat string
	flagNoInput     bool
	flagDebug       bool

	store *store.Store
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	app := &App{Version: version, Stdin: stdin, Stdout: stdout, Stderr: stderr}
	root := app.rootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		if cmd != nil {
			app.maybeUpdate(ctx, cmd)
		}
		return 0
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		fmt.Fprintln(stderr, "vinted: interrupted")
		return 130
	}
	if app.store != nil {
		app.store.Close()
	}
	path := "vinted"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	ef := output.ErrorFormat(app.flagErrorFormat)
	if v := os.Getenv("VINTED_ERROR_FORMAT"); v != "" && app.flagErrorFormat == "text" {
		ef = output.ErrorFormat(v)
	}
	return output.PrintError(stderr, err, ef, version, path)
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "vinted",
		Short: "Search, track and chat on Vinted from the terminal",
		Long: `Search, track and chat on Vinted from the terminal, with your own account.

Docs: https://vinted.micr.dev/docs

Output is JSON by default so every command pipes into jq. Use --format pretty
for humans. Errors go to stderr; exit codes are documented in docs/cli-spec.md.


Examples:
  vinted auth login
  vinted search "levis 501" --price-to 30 --order newest_first --format pretty
  vinted watch add search "levis 501" --price-to 30 --name levis --notify phone
  vinted chat list --unread --format pretty

Agent usage: vinted skills get vinted-usage`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       a.Version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return a.setup(cmd)
		},
	}
	root.SetVersionTemplate("vinted {{.Version}}\n")
	root.CompletionOptions.HiddenDefaultCmd = false
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return output.Usagef("%s. See `%s --help`", err.Error(), cmd.CommandPath())
	})

	pf := root.PersistentFlags()
	pf.StringVar(&a.flagProfile, "profile", "", "profile to act as (env VINTED_PROFILE)")
	pf.StringVar(&a.flagFormat, "format", "json", "output format: json, jsonl, pretty, toon")
	pf.BoolVar(&a.flagNoColor, "no-color", false, "disable colour in pretty output (also NO_COLOR)")
	pf.StringVar(&a.flagErrorFormat, "error-format", "text", "error rendering on stderr: text or json")
	pf.BoolVar(&a.flagNoInput, "no-input", false, "never prompt; fail when input is missing")
	pf.BoolVar(&a.flagDebug, "debug", false, "log every http request to stderr (secrets redacted)")
	_ = root.RegisterFlagCompletionFunc("format", cobra.FixedCompletions([]string{"json", "jsonl", "pretty", "toon"}, cobra.ShellCompDirectiveNoFileComp))
	_ = root.RegisterFlagCompletionFunc("error-format", cobra.FixedCompletions([]string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp))

	root.AddCommand(
		a.authCmd(), a.profileCmd(),
		a.searchCmd(), a.categoryCmd(), a.brandCmd(), a.alertCmd(),
		a.itemCmd(), a.userCmd(), a.meCmd(),
		a.chatCmd(),
		a.mcpCmd(),
		a.watchCmd(), a.sinkCmd(),
		a.configCmd(), a.doctorCmd(), a.cooldownCmd(), a.skillsCmd(), a.updateCmd(),
	)
	strictArgs(root)
	return root
}

// setup loads config and credentials and builds the client. It runs for every
// command, so it must stay cheap: no network.
func (a *App) setup(cmd *cobra.Command) error {
	if cmd.Name() == "help" || cmd.Name() == "completion" || (cmd.Parent() != nil && cmd.Parent().Name() == "completion") {
		return nil
	}
	format, err := output.ParseFormat(a.flagFormat)
	if err != nil {
		return err
	}
	if a.flagErrorFormat != "text" && a.flagErrorFormat != "json" {
		return output.Usagef("--error-format must be text or json")
	}
	stdoutFile, _ := a.Stdout.(*os.File)
	stdinFile, _ := a.Stdin.(*os.File)
	stderrFile, _ := a.Stderr.(*os.File)
	a.Interactive = !a.flagNoInput && output.IsTerminal(stdinFile) && output.IsTerminal(stdoutFile) && output.IsTerminal(stderrFile)
	a.Printer = output.Printer{W: a.Stdout, Format: format, Color: output.ColorEnabled(a.flagNoColor, stdoutFile)}

	if cmd.Name() == "update" {
		return nil
	}
	a.Paths = config.DefaultPaths()
	a.Cfg, err = config.Load(a.Paths.ConfigFile)
	if err != nil {
		return err
	}
	a.Creds, err = config.LoadCredentials(a.Paths.CredentialsFile)
	if err != nil {
		return err
	}
	a.loadedCreds = config.CloneCredentials(a.Creds)
	a.Profile = config.ResolveProfile(a.flagProfile, a.Cfg)

	a.Client = vinted.New()
	base := a.Client.Base
	if d := a.Cfg.Profiles[a.Profile].Domain; d != "" && os.Getenv("VINTED_BASE_URL") == "" {
		base = "https://" + d
	}
	a.useSite(base)
	if a.flagDebug {
		a.Client.Debug = a.Stderr
	}
	a.Client.Notice = func(s string) { fmt.Fprintln(a.Stderr, "vinted: "+s) }
	return a.attachSession()
}

// throttleClock is the gate's time source; tests replace it so pacing and
// cooldowns run on a fake clock.
var throttleClock = throttle.System

// useSite points the client at a Vinted site, routes it through the
// configured proxy, and puts every request behind the site's shared gate. A
// bad request_interval or proxy does not fail here: it fails every request
// (nothing goes out, least of all direct), so `vinted config set` can still
// fix it.
func (a *App) useSite(base string) {
	a.Client.Base = base
	err := a.applyProxy()
	gap, gapErr := a.Cfg.RequestGap()
	if gapErr == nil && gap != 0 && gap < throttle.MinInterval {
		gapErr = fmt.Errorf("%s is below the floor of %s", gap, throttle.MinInterval)
	}
	if err == nil && gapErr != nil {
		err = output.Usagef("config request_interval: %v. Fix it with `vinted config set request_interval 2s`", gapErr)
	}
	a.Gate = throttle.New(a.Paths.StateDir, base, throttle.Options{Interval: gap, Clock: throttleClock, Err: err})
	a.Client.Gate, a.Client.Tokens = a.Gate, a.Gate
}

// applyProxy routes all Vinted traffic through VINTED_PROXY, else config's
// proxy, with no fallback to a direct connection.
func (a *App) applyProxy() error {
	raw, src := os.Getenv("VINTED_PROXY"), "VINTED_PROXY"
	if raw == "" {
		raw, src = a.Cfg.Proxy, "config proxy"
	}
	a.Proxy = ""
	if raw == "" {
		return nil
	}
	u, err := vinted.ParseProxy(raw)
	if err != nil {
		return output.Usagef("%s: %v. Nothing is sent to vinted until it is fixed", src, err)
	}
	a.Client.UseProxy(u)
	a.Proxy = vinted.ProxyLabel(u)
	return nil
}

// attachSession wires the active profile's cookie jar (or VINTED_COOKIES) into
// the client. Commands that need auth call requireSession. Refreshed cookies
// are written back under the credentials lock; env cookies never touch disk.
func (a *App) attachSession() error {
	if raw := os.Getenv("VINTED_COOKIES"); raw != "" {
		cookies, _, err := vinted.ParseCookieExport(strings.NewReader(raw))
		if err != nil {
			return output.Usagef("VINTED_COOKIES: %v", err)
		}
		a.Session = vinted.NewSession(a.Client, cookies)
		a.Client.Session = a.Session
		return nil
	}
	s, ok := a.Creds.Profiles[a.Profile]
	if !ok || len(s.Cookies) == 0 {
		return nil
	}
	a.Session = vinted.NewSession(a.Client, s.Cookies)
	if !s.SessionExpires.IsZero() {
		a.Session.Expires = s.SessionExpires
	}
	path, profile := a.Paths.CredentialsFile, a.Profile
	a.Session.OnRotate = func(cookies map[string]string, expires time.Time) {
		next, err := config.RotateSession(path, profile, cookies, expires)
		if err != nil {
			fmt.Fprintf(a.Stderr, "vinted: could not save the rotated session: %v\n", err)
			return
		}
		a.adoptCreds(next)
	}
	a.Session.Store = credStore{app: a, path: path, profile: profile}
	a.Client.Session = a.Session
	return nil
}

func (a *App) adoptCreds(next config.Credentials) {
	a.Creds = next
	a.loadedCreds = config.CloneCredentials(next)
}

// credStore lets the session refresh under the credentials lock. Vinted
// rotates refresh_token_web on every refresh and revokes the login when an old
// one is replayed, so two processes (the watch timer and a command) must never
// refresh with the same token: the second one takes the lock, finds the first
// one's fresh jar on disk and adopts it instead.
type credStore struct {
	app           *App
	path, profile string
}

func (s credStore) Lock(ctx context.Context) (func(), error) {
	return config.LockCredentials(ctx, s.path)
}

func (s credStore) Load() (map[string]string, error) {
	creds, err := config.LoadCredentials(s.path)
	if err != nil {
		return nil, err
	}
	return creds.Profiles[s.profile].Cookies, nil
}

func (s credStore) Save(cookies map[string]string, expires time.Time) error {
	next, err := config.RotateSessionLocked(s.path, s.profile, cookies, expires)
	if err != nil {
		return err
	}
	s.app.adoptCreds(next)
	return nil
}

func (a *App) requireSession() error {
	if a.Session == nil {
		return &vinted.Error{Kind: vinted.KindAuth, Msg: fmt.Sprintf("profile %q has no session. Run `vinted auth login`", a.Profile)}
	}
	return nil
}

// userID returns the logged-in account's id: stored at login, read from the
// access token, or fetched once as a last resort.
func (a *App) userID(ctx context.Context) (int64, error) {
	if s, ok := a.Creds.Profiles[a.Profile]; ok && s.UserID != 0 && os.Getenv("VINTED_COOKIES") == "" {
		return s.UserID, nil
	}
	if id := a.Session.UserID(); id != 0 {
		return id, nil
	}
	me, err := a.Client.Me(ctx)
	if err != nil {
		return 0, err
	}
	return me.ID, nil
}

func (a *App) Store() (*store.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	st, err := store.Open(a.Paths.StateDB)
	if err != nil {
		return nil, err
	}
	a.store = st
	return st, nil
}

// confirm asks a yes/no question when interactive. Non-interactive callers
// must pass --yes; otherwise this is a usage error, never a silent proceed.
func (a *App) confirm(yes bool, prompt string) error {
	if yes {
		return nil
	}
	if !a.Interactive {
		return output.Usagef("%s Pass --yes to confirm without a prompt", prompt)
	}
	fmt.Fprintf(a.Stderr, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	if s := strings.ToLower(strings.TrimSpace(line)); s != "y" && s != "yes" {
		return output.Usagef("cancelled")
	}
	return nil
}

// readArgOrStdin returns the text argument, or stdin when the argument is "-".
func (a *App) readArgOrStdin(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	raw, err := io.ReadAll(a.Stdin)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", output.Usagef("stdin was empty")
	}
	return text, nil
}

// saveConfig writes config.toml back.
func (a *App) saveConfig() error { return config.Save(a.Paths.ConfigFile, a.Cfg) }

func (a *App) saveCreds() error {
	next, err := config.MergeCredentials(a.Paths.CredentialsFile, a.loadedCreds, a.Creds)
	if err == nil {
		a.Creds = next
		a.loadedCreds = config.CloneCredentials(next)
	}
	return err
}
