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
	want := map[string]string{}
	for _, kv := range env {
		want[kv[0]] = kv[1]
	}
	// Every variable a unit would carry, set here or not: one the shell
	// lacks but the saved environment has would still reach the task.
	keys := append(append(append([]string{}, serviceEnvXDGKeys...), "VINTED_CONFIG"), serviceEnvURLKeys...)
	for _, key := range keys {
		saved, _, err := k.GetStringValue(key)
		if err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		if saved != want[key] {
			return output.Usagef("the logon task starts with your saved user environment, where %s differs from this shell. Make them match (`setx %s <value>`, or remove it from the saved environment, then open a new terminal) and enable again", key, key)
		}
	}
	return nil
}
