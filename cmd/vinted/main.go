// Command vinted is a terminal client for Vinted. See docs/cli-spec.md.
// It lives under cmd/ so that `go install .../cmd/vinted` names the binary vinted.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"github.com/Microck/vinted-cli/internal/cli"
)

// version is set by goreleaser through -ldflags "-X main.version=...".
var version string

func main() {
	os.Exit(cli.Execute(resolveVersion(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// resolveVersion prefers the ldflags value. Without it (`go install ...@vX.Y.Z`,
// or a plain `go build` inside the repo, which Go stamps with a VCS
// pseudo-version) it uses the module version recorded in the binary, so
// `--version` still identifies the build. "dev" is the last resort.
func resolveVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}
