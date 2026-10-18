package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/throttle"
)

type cooldownView throttle.Status

func (v cooldownView) Pretty(w io.Writer, color bool) {
	rows := [][]string{{"site", v.Site}}
	if v.Blocked {
		rows = append(rows,
			[]string{"state", "cooling down after a block"},
			[]string{"blocked at", v.BlockedAt.Local().Format("2006-01-02 15:04 MST")},
			[]string{"until", v.BlockedUntil.Local().Format("2006-01-02 15:04 MST") + " " + output.Dim("("+v.Remaining+" left)", color)},
			[]string{"cause", v.Cause})
	} else {
		rows = append(rows, []string{"state", "no cooldown"})
	}
	rows = append(rows, []string{"request interval", v.RequestInterval}, []string{"state file", v.File})
	output.Table(w, color, nil, rows)
}

// clearedView is what `cooldown clear` prints: the cooldown that was lifted.
type clearedView struct {
	Cleared bool            `json:"cleared"`
	Was     throttle.Status `json:"was"`
}

func (v clearedView) Pretty(w io.Writer, color bool) {
	if !v.Cleared {
		fmt.Fprintf(w, "%s had no cooldown\n", v.Was.Site)
		return
	}
	fmt.Fprintf(w, "lifted the cooldown on %s (%s were left)\n", v.Was.Site, v.Was.Remaining)
}

func (a *App) cooldownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cooldown",
		Short: "Show or lift the pause after Vinted blocked a request",
		Long: `When Vinted's bot protection (Datadome/Cloudflare) blocks a request, the CLI
stops contacting that site for 6 hours (longer if Vinted's Retry-After asks
for it), in every process and profile on this machine: retrying while blocked
only extends the block. Commands fail at once with exit 5 and the time left,
without sending anything.

"vinted cooldown" shows the state for the profile's site. "vinted cooldown
clear" lifts it early. Only do that once Vinted works normally in your browser
again (solve any captcha there, then re-export cookies with "vinted auth login"
if the session was blocked).

Examples:
  vinted cooldown
  vinted cooldown clear --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.Gate.Status()
			if err != nil {
				return err
			}
			return a.Printer.Print(cooldownView(st))
		},
	}
	var yes bool
	clear := &cobra.Command{
		Use:   "clear",
		Short: "Lift the cooldown for the profile's site now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.Gate.Status()
			if err == nil && st.Blocked {
				if err := a.confirm(yes, fmt.Sprintf("%s of the pause on %s are left. Lift it now? Only do this if vinted works normally in your browser.", st.Remaining, st.Site)); err != nil {
					return err
				}
			}
			was, err := a.Gate.Clear(cmd.Context())
			if err != nil {
				return err
			}
			return a.Printer.Print(clearedView{Cleared: was.Blocked, Was: was})
		},
	}
	clear.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	cmd.AddCommand(clear)
	return cmd
}
