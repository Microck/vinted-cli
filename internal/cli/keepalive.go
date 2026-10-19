package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/galleton"
	"github.com/Microck/vinted-cli/internal/output"
)

// renewalWarnWindow is how close the refresh token's expiry gets before
// `auth status` and `doctor` suggest the renewal service.
const renewalWarnWindow = 48 * time.Hour

// promptTerminal reports whether the login may ask about the renewal
// service: stdin and stderr are terminals. Tests replace it.
var promptTerminal = func(a *App) bool {
	in, _ := a.Stdin.(*os.File)
	errf, _ := a.Stderr.(*os.File)
	return output.IsTerminal(in) && output.IsTerminal(errf)
}

const serviceHint = "run `vinted auth service enable` later to turn it on"

// ciMode is true on CI runners, where nothing should be installed unasked.
func ciMode() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CI")))
	return v == "true" || v == "1"
}

// renewalInstalled reports whether the renewal service is installed for
// this state directory. An unsupported OS counts as not installed.
func (a *App) renewalInstalled() bool {
	u, _, err := a.authServiceUnit()
	return err == nil && u.installed()
}

// offerRenewalService runs after a successful login. want is "on" for
// --service, "off" for --no-service and "" to ask in a terminal.
func (a *App) offerRenewalService(ctx context.Context, want string) error {
	switch want {
	case "off":
		return nil
	case "on":
		// Always (re)enable: a unit file left by an enable that failed
		// half-way counts as installed, and enabling again retries it.
		if err := a.enableRenewal(ctx); err != nil {
			return fmt.Errorf("logged in, but background renewal could not be enabled: %w", err)
		}
		return nil
	}
	if a.flagNoInput || ciMode() || galleton.Configured() || !promptTerminal(a) || a.renewalInstalled() {
		return nil
	}
	fmt.Fprint(a.Stderr, "keep this login alive in the background? [Y/n] ")
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if err != nil && answer == "" {
		// End of input (Ctrl-D) or a read error is no answer, so not a yes.
		answer = "n"
	}
	switch answer {
	case "", "y", "yes":
		if err := a.enableRenewal(ctx); err != nil {
			fmt.Fprintf(a.Stderr, "vinted: background renewal could not be enabled: %v\n%s\n", err, serviceHint)
		}
	default:
		fmt.Fprintln(a.Stderr, serviceHint)
	}
	return nil
}

// enableRenewal is `vinted auth service enable`, reported on stderr so
// stdout keeps the login's status.
func (a *App) enableRenewal(ctx context.Context) error {
	u, _, err := a.authServiceUnit()
	if err != nil {
		return err
	}
	fresh := !u.installed()
	res, err := a.authServiceApply(ctx, true)
	if err != nil {
		// An enable that fails half-way keeps the unit file for a retry. One
		// this login wrote would read as installed while nothing renews, so
		// undo it the way `auth service disable` does (which unregisters a
		// Windows task before its marker, and keeps the file if that fails):
		// the next login asks again and the warnings stay.
		if fresh && u.installed() {
			if _, derr := a.authServiceApply(ctx, false); derr != nil {
				// The unit stays (a Windows marker is what lets disable find
				// its task), so say it is half-installed, not on.
				return fmt.Errorf("%w; undoing it failed too (%v), so run `vinted auth service disable` to clean up", err, derr)
			}
		}
		return err
	}
	fmt.Fprintf(a.Stderr, "background renewal on (%v %v)\n", res["platform"], res["unit"])
	return nil
}

// renewalWarning is set when a stored login's refresh token expires within
// renewalWarnWindow and no renewal service keeps it alive between commands.
func (a *App) renewalWarning(s config.Session) string {
	if galleton.Configured() || s.GalletonID == "" || s.RefreshExpires.IsZero() || time.Until(s.RefreshExpires) > renewalWarnWindow || a.renewalInstalled() {
		return ""
	}
	verb := "expires"
	if !time.Now().Before(s.RefreshExpires) {
		verb = "expired"
	}
	return fmt.Sprintf("the refresh token from the login %s %s and no background renewal is installed; run `vinted auth service enable`", verb, s.RefreshExpires.Local().Format("2006-01-02 15:04 MST"))
}

// printStatus prints `auth status`, with its warning on stderr too.
func (a *App) printStatus(st authStatus) error {
	if st.Warning != "" {
		fmt.Fprintln(a.Stderr, "vinted: warning: "+st.Warning)
	}
	return a.Printer.Print(st)
}

// noteRenewal moves the recorded refresh token expiry forward when the
// engine reports a renewal after the one it was recorded for: each renewal
// issues a new refresh token, assumed to live as long as the recorded one.
func (a *App) noteRenewal(last time.Time) {
	s, ok := a.Creds.Profiles[a.Profile]
	if !ok || s.RefreshExpires.IsZero() || s.RefreshIssued.IsZero() || !last.After(s.RefreshIssued) {
		return
	}
	life := s.RefreshExpires.Sub(s.RefreshIssued)
	if life <= 0 {
		return
	}
	s.RefreshIssued, s.RefreshExpires = last.UTC(), last.Add(life).UTC()
	a.Creds.Profiles[a.Profile] = s
	if err := a.saveCreds(); err != nil {
		fmt.Fprintf(a.Stderr, "vinted: could not record the session renewal: %v\n", err)
	}
}
