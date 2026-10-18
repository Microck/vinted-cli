package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/output"
)

// CommandTree returns the whole command tree, wired but never executed, so
// tools outside the package can walk it. The docs generator uses it to build
// the command reference from the binary rather than from a copy that drifts.
func CommandTree(version string) *cobra.Command {
	app := &App{Version: version}
	return app.rootCmd()
}

// groupAnnotation marks a noun that only holds subcommands. Its RunE exists to
// reject unknown subcommands, not to do anything, so the docs show no usage
// line for it.
const groupAnnotation = "vinted:group"

// IsGroup reports whether cmd is a noun that only holds subcommands.
func IsGroup(cmd *cobra.Command) bool { return cmd.Annotations[groupAnnotation] == "true" }

// strictArgs makes cobra's own input errors usage errors (exit 2), as the
// exit-code table promises: a wrong argument count, and an unknown
// subcommand. Cobra reports the first as a plain error (exit 1) and the second
// not at all below the root: `vinted watch lsit` would print help and exit 0,
// so a typo in a script passes silently. Flag errors already go through
// SetFlagErrorFunc.
func strictArgs(cmd *cobra.Command) {
	if !cmd.HasParent() {
		// A group's RunE makes cobra call it runnable, which would add a
		// meaningless "vinted watch [flags]" line to its usage. The template is
		// inherited by every command, so the root sets it once.
		cmd.SetUsageTemplate(strings.Replace(cmd.UsageTemplate(), "Usage:{{if .Runnable}}",
			`Usage:{{if and .Runnable (not (index .Annotations "`+groupAnnotation+`"))}}`, 1))
	}
	switch {
	case cmd.HasSubCommands() && cmd.Run == nil && cmd.RunE == nil:
		if cmd.Annotations == nil {
			cmd.Annotations = map[string]string{}
		}
		cmd.Annotations[groupAnnotation] = "true"
		cmd.Args = cobra.ArbitraryArgs
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			msg := fmt.Sprintf("unknown command %q for %q.", args[0], cmd.CommandPath())
			if cmd.SuggestionsMinimumDistance <= 0 {
				cmd.SuggestionsMinimumDistance = 2
			}
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				msg += " Did you mean " + strings.Join(s, " or ") + "?"
			}
			return output.Usagef("%s See `%s --help`", msg, cmd.CommandPath())
		}
	case cmd.Args != nil:
		validate := cmd.Args
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if err := validate(cmd, args); err != nil {
				return output.Usagef("%v. See `%s --help`", err, cmd.CommandPath())
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		strictArgs(sub)
	}
}
