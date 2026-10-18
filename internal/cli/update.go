package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/updater"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/spf13/cobra"
)

func (a *App) updateCmd() *cobra.Command {
	var yes, check bool
	cmd := &cobra.Command{
		Use: "update", Short: "Update vinted to the latest stable release",
		Long: `Check GitHub for the latest stable release and update this installation.
Package-manager installations use their package manager. Direct binaries are
verified against release checksums before replacement. Use --check to only
check, or --yes to update without a prompt. Development builds are not replaced.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !updater.Stable(a.Version) {
				return output.Usagef("development or prerelease build; install a stable release before using update")
			}
			r, err := updater.Latest(cmd.Context(), updater.Client())
			if err != nil {
				return updateCheckError(err, a.Version)
			}
			available := updater.Newer(r.Tag, a.Version)
			if check || !available {
				return a.Printer.Print(map[string]any{"current_version": a.Version, "latest_version": r.Tag, "update_available": available})
			}
			if !yes {
				if !a.Interactive {
					return output.Usagef("%s available; run `vinted update --yes` to update", r.Tag)
				}
				if !a.updateConsent(r.Tag) {
					return nil
				}
			}
			return a.installUpdate(cmd.Context(), r)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "update without asking for confirmation")
	cmd.Flags().BoolVar(&check, "check", false, "check for a new release without installing")
	return cmd
}

// updateCheckError turns a failed release lookup into an error with the right
// exit code: no published release yet is "not found" (4), not a crash (1),
// and an unreachable GitHub is a network error (6) worth retrying.
func updateCheckError(err error, version string) error {
	if errors.Is(err, updater.ErrNoRelease) {
		return &vinted.Error{Kind: vinted.KindNotFound, Msg: fmt.Sprintf("%v (%s), so there is nothing to update to; this is vinted %s", err, updater.ReleasesPage, version)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return &vinted.Error{Kind: vinted.KindNetwork, Retryable: true, Msg: "could not reach GitHub to check for a release: " + err.Error()}
	}
	return err
}

func (a *App) updateConsent(tag string) bool {
	fmt.Fprintf(a.Stderr, "vinted %s is available (current %s). Update? [Y/n] ", strings.TrimPrefix(tag, "v"), a.Version)
	// Read only this line so subsequent interactive commands keep their input.
	var input [1]byte
	var line strings.Builder
	for {
		_, err := io.ReadFull(a.Stdin, input[:])
		b := input[0]
		if err != nil {
			return false
		}
		if b == '\n' {
			break
		}
		line.WriteByte(b)
	}
	answer := strings.ToLower(strings.TrimSpace(line.String()))
	return answer == "" || answer == "y" || answer == "yes"
}

func (a *App) installUpdate(ctx context.Context, r updater.Release) error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if command := updater.Manager(path); command != nil {
		fmt.Fprintln(a.Stderr, "Updating with", strings.Join(command, " "))
		if command[0] == "yay" {
			if _, err := exec.LookPath("yay"); err != nil {
				if _, err := exec.LookPath("paru"); err == nil {
					command[0] = "paru"
				}
			}
		}
		c := exec.CommandContext(ctx, command[0], command[1:]...)
		if command[0] == "scoop" {
			c = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", "scoop update vinted")
		}
		c.Stdin, c.Stdout, c.Stderr = a.Stdin, a.Stderr, a.Stderr
		if err = c.Run(); err != nil {
			return fmt.Errorf("package-manager update failed; run `%s`: %w", strings.Join(command, " "), err)
		}
		fmt.Fprintln(a.Stderr, "Update finished. Run `vinted --version` to verify the installed version.")
		return nil
	}
	if err = updater.Install(ctx, updater.Client(), r, path); err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "Updated to vinted %s.\n", strings.TrimPrefix(r.Tag, "v"))
	return nil
}

// maybeUpdate runs after a successful interactive invocation. Failed checks
// never change command output or exit status; automation never checks or prompts.
func (a *App) maybeUpdate(ctx context.Context, cmd *cobra.Command) {
	if !a.Interactive || !updater.Stable(a.Version) || os.Getenv("VINTED_NO_UPDATE_CHECK") != "" {
		return
	}
	top := cmd
	for top.Parent() != nil && top.Parent().Parent() != nil {
		top = top.Parent()
	}
	switch top.Name() {
	case "update", "mcp", "completion", "help", "auth", "watch":
		return
	}
	cache := filepath.Join(a.Paths.StateDir, "update-check")
	if info, err := os.Stat(cache); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return
	}
	if os.MkdirAll(a.Paths.StateDir, 0700) != nil {
		return
	}
	// Cache attempts as well as successful checks to avoid repeated offline delays.
	if os.WriteFile(cache, []byte(time.Now().UTC().Format(time.RFC3339)), 0600) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := updater.Latest(ctx, updater.Client())
	if err != nil || !updater.Newer(r.Tag, a.Version) {
		return
	}
	if a.updateConsent(r.Tag) {
		if err := a.installUpdate(cmd.Context(), r); err != nil {
			fmt.Fprintln(a.Stderr, "vinted: update failed:", err)
		}
	}
}
