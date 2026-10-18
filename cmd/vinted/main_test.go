package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryContract builds the real executable and checks the parts of the
// contract that only exist at process level: --version on stdout, --help exit
// 0, and non-zero exit codes reaching the shell.
func TestBinaryContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "vinted")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=9.9.9-test", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	env := append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir(), "XDG_DATA_HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir(), "VINTED_PROFILE=")

	run := func(args ...string) (string, string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return out.String(), errb.String(), code
	}

	if out, _, code := run("--version"); code != 0 || strings.TrimSpace(out) != "vinted 9.9.9-test" {
		t.Fatalf("--version: %d %q", code, out)
	}
	if out, _, code := run("--help"); code != 0 || !strings.Contains(out, "Examples:") {
		t.Fatalf("--help: %d %q", code, out)
	}
	if _, errOut, code := run("chat", "list"); code != 3 || !strings.Contains(errOut, "auth login") {
		t.Fatalf("no session should exit 3: %d %q", code, errOut)
	}
	if _, errOut, code := run("search", "--bogus"); code != 2 || !strings.Contains(errOut, "unknown flag") {
		t.Fatalf("bad flag should exit 2: %d %q", code, errOut)
	}
	if out, _, code := run("completion", "zsh"); code != 0 || !strings.Contains(out, "compdef") {
		t.Fatalf("completion: %d", code)
	}
}
