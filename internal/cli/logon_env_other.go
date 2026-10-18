//go:build !windows

package cli

// checkLogonEnv only matters for Windows logon tasks; systemd units and
// LaunchAgents carry the environment in their definition.
func checkLogonEnv([][2]string) error { return nil }
