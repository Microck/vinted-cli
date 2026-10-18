package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/mcp"
	"github.com/Microck/vinted-cli/internal/output"
)

func (a *App) mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the read commands to an agent harness over MCP",
		Long: `Run a Model Context Protocol server on stdin/stdout.

Tools mirror the CLI: each one runs the command it is named after and returns
the same JSON. Only reads are tools: favoriting and sending messages stay with
the person at the terminal.

Tools: ` + strings.Join(mcp.ToolNames(), ", ") + `

Examples:
  vinted mcp install claude-code
  vinted mcp install codex --profile work
  vinted mcp                       # usually started by the harness, not by hand`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := &mcp.Server{Version: a.Version, Run: a.runCommand}
			return srv.Serve(cmd.Context(), a.Stdin, a.Stdout)
		},
	}
	cmd.AddCommand(a.mcpInstallCmd())
	return cmd
}

func (a *App) mcpInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "install " + strings.Join(mcp.Harnesses, "|"),
		Short:     "Write the server entry into a harness's config, idempotently",
		Args:      cobra.ExactArgs(1),
		ValidArgs: mcp.Harnesses,
		Long: `Add this binary as an MCP server to an agent harness.

The entry points at the running executable, so a binary outside PATH still
works. Other entries in the file are left alone, comments included, and a
second run reports "unchanged".

Files written: ~/.claude.json (claude-code), ~/.codex/config.toml (codex),
~/.cursor/mcp.json (cursor).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !slices.Contains(mcp.Harnesses, args[0]) {
				return output.Usagef("unknown harness %q. Choose one of %s", args[0], strings.Join(mcp.Harnesses, ", "))
			}
			exe, err := os.Executable()
			if err != nil || exe == "" {
				exe = "vinted" // fall back to PATH lookup by the harness
			}
			// The profile is part of the server identity: a harness installed
			// for a chosen profile must keep acting as that account. The flag
			// and VINTED_PROFILE both choose one; a bare install stays
			// unpinned so the config default keeps applying, including after
			// a later `vinted profile use`.
			serverArgs := []string{"mcp"}
			if chosen := config.ResolveProfile(a.flagProfile, config.Config{}); a.flagProfile != "" || os.Getenv("VINTED_PROFILE") != "" {
				serverArgs = append(serverArgs, "--profile", chosen)
			}
			change, err := mcp.Install(args[0], exe, serverArgs)
			if err != nil {
				return err
			}
			return a.Printer.Print(installView(change))
		},
	}
}

type installView mcp.Change

func (v installView) Pretty(w io.Writer, color bool) {
	fmt.Fprintf(w, "%s %s\n", v.Action, v.Path)
	fmt.Fprintln(w, output.Dim(v.Command+" "+strings.Join(v.Args, " "), color))
}

// runCommand executes one CLI invocation inside this process and reports what
// it printed. Tools are defined as argv, so what the agent reads is literally
// what the command prints; there is no second rendering path to drift.
//
// Each call builds a fresh App: config, credentials and a rotated session are
// re-read, so a long-running server does not serve a stale account.
func (a *App) runCommand(ctx context.Context, argv []string) mcp.Result {
	var out bytes.Buffer
	sub := &App{
		Version: a.Version,
		Stdin:   strings.NewReader(""),
		Stdout:  &out,
		// Notices (rate-limit warnings, "could not mark as read") belong on the
		// server's stderr, which the harness logs; stdout is the transport.
		Stderr: a.Stderr,
	}
	root := sub.rootCmd()
	// --no-input: nothing here has a terminal to prompt at. The server's own
	// profile carries over, so a harness started with --profile keeps acting
	// as that account.
	global := []string{"--no-input"}
	if a.flagProfile != "" {
		global = append(global, "--profile", a.flagProfile)
	}
	root.SetArgs(append(global, argv...))
	root.SetIn(sub.Stdin)
	root.SetOut(sub.Stdout)
	root.SetErr(sub.Stderr)

	cmd, err := root.ExecuteContextC(ctx)
	if sub.store != nil {
		sub.store.Close() // one server run does many checks; do not leak handles
	}
	if err == nil {
		return mcp.Result{Stdout: out.String()}
	}
	path := "vinted"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	var envelope bytes.Buffer
	output.PrintError(&envelope, err, output.ErrJSON, a.Version, path)
	// Whatever was printed before the failure still goes back: `watch check`
	// reports the events it produced and then the check that broke.
	return mcp.Result{Stdout: out.String(), ErrorJSON: envelope.String()}
}
