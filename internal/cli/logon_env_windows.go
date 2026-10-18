//go:build windows

package cli

import (
	"errors"

	"golang.org/x/sys/windows/registry"

	"github.com/Microck/vinted-cli/internal/output"
)

// checkLogonEnv refuses a logon task that would start with a different
// environment from this shell. A scheduled task sees the user's saved
// environment (HKCU\Environment), not the variables of the shell that created
// it, so a VINTED_PROXY set only here would leave the task renewing directly.
// HOME is skipped: on Windows the paths come from the user profile.
func checkLogonEnv(env [][2]string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for _, kv := range env {
		if kv[0] == "HOME" {
			continue
		}
		saved, _, err := k.GetStringValue(kv[0])
		if err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		if saved != kv[1] {
			return output.Usagef("the logon task starts with your saved user environment, where %s is not what this shell has. Save it with `setx %s <value>` and open a new terminal, or unset it here, then enable again", kv[0], kv[0])
		}
	}
	return nil
}
