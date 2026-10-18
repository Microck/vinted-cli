package cli

// Unit-file rendering is tested from inside the package because installing for
// real needs a running systemd or launchd. The launchd and Windows cases can
// only be checked this way on CI and on the author's Linux box, so they are
// checked closely: a plist that does not parse, or a schtasks line with the
// wrong quoting, fails silently on the platform that runs it.

import (
	"encoding/xml"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/config"
)

var checkArgs = []string{"watch", "check", "--all", "--profile", "default", "--format", "jsonl"}

func TestServiceEnvCarriesThePathVariablesButNotTheEngineToken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/home/u/state")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("VINTED_GALLETON_TOKEN_FILE", "/secret/api.token")
	got := map[string]string{}
	for _, kv := range serviceEnv() {
		got[kv[0]] = kv[1]
	}
	if got["XDG_STATE_HOME"] != "/home/u/state" {
		t.Fatalf("XDG_STATE_HOME not carried: %v", got)
	}
	if _, ok := got["XDG_DATA_HOME"]; ok {
		t.Fatal("an unset variable must stay unset in the unit, not become an empty path")
	}
	if _, ok := got["VINTED_GALLETON_TOKEN_FILE"]; ok {
		t.Fatal("the engine's API token location must never be written into a unit file")
	}
}

func TestSystemdUnitPinsTheInstallEnvironmentAndQuotesTheExecutable(t *testing.T) {
	env := [][2]string{{"XDG_DATA_HOME", "/home/u/my data"}}
	service, timer := systemdUnits("default", "/opt/my apps/vinted", checkArgs, "/home/u/state/w.log", 10*time.Minute, env)
	if !strings.Contains(service, `Environment="XDG_DATA_HOME=/home/u/my data"`) {
		t.Fatalf("the unit must pin the state location it was installed with:\n%s", service)
	}
	if !strings.Contains(service, `ExecStart="/opt/my apps/vinted" watch check --all`) {
		t.Fatalf("ExecStart must quote the executable:\n%s", service)
	}
	if !strings.Contains(service, "StandardOutput=append:/home/u/state/w.log") {
		t.Fatalf("output must be appended to the state-directory log:\n%s", service)
	}
	if !strings.Contains(timer, "OnUnitActiveSec=10min") || !strings.Contains(timer, "WantedBy=timers.target") {
		t.Fatalf("timer must repeat at the interval and be installable:\n%s", timer)
	}
}

// systemd expands % specifiers inside unit values: an unknown one makes it
// drop the whole assignment, a known one substitutes something else. Either
// way the scheduled check would reach the wrong place, silently.
func TestSystemdUnitDoublesPercentAgainstSpecifierExpansion(t *testing.T) {
	env := [][2]string{{"VINTED_BASE_URL", "https://example.test/%2Fpath"}}
	service, _ := systemdUnits("default", "/opt/100%cool/vinted", checkArgs, "/home/u/50% full/w.log", time.Minute, env)
	for _, want := range []string{
		`Environment="VINTED_BASE_URL=https://example.test/%%2Fpath"`,
		`ExecStart="/opt/100%%cool/vinted"`,
		"append:/home/u/50%% full/w.log",
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("unit does not escape %%:\n%s", service)
		}
	}
	if strings.Contains(strings.ReplaceAll(service, "%%", ""), "%") {
		t.Fatalf("a bare %% is left somewhere in the unit:\n%s", service)
	}
}

// A relative XDG value is invalid per the spec and adrg/xdg ignores it, so the
// install used the HOME default. Pinning it would make the timer obey an
// override the install did not: a different, empty state database.
func TestServiceEnvDropsRelativeXDGOverrides(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "reldata")
	t.Setenv("XDG_STATE_HOME", "/abs/state")
	for _, kv := range serviceEnv() {
		if kv[0] == "XDG_DATA_HOME" {
			t.Fatalf("a relative XDG value must not be pinned, got %q", kv[1])
		}
	}
	found := false
	for _, kv := range serviceEnv() {
		found = found || (kv[0] == "XDG_STATE_HOME" && kv[1] == "/abs/state")
	}
	if !found {
		t.Fatal("an absolute XDG value must still be pinned")
	}
}

// VINTED_CONFIG is read raw rather than through xdg, so a relative one does
// take effect, and the scheduler runs the job from its own directory.
func TestServiceEnvMakesPathOverridesAbsolute(t *testing.T) {
	t.Setenv("VINTED_CONFIG", "custom/config.toml")
	want, err := filepath.Abs("custom/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range serviceEnv() {
		if kv[0] != "VINTED_CONFIG" {
			continue
		}
		if kv[1] != want {
			t.Fatalf("relative config override not resolved: got %q, want %q", kv[1], want)
		}
		return
	}
	t.Fatal("VINTED_CONFIG missing from the pinned environment")
}

// HOME is where every default path comes from when the XDG variables are
// unset, so a unit that does not carry it can land on another home.
func TestServiceEnvCarriesHome(t *testing.T) {
	t.Setenv("HOME", "/home/someone-else")
	for _, kv := range serviceEnv() {
		if kv[0] == "HOME" && kv[1] == "/home/someone-else" {
			return
		}
	}
	t.Fatal("HOME must be pinned into the unit")
}

// A relative $HOME makes every derived path relative, and systemd ignores a
// relative StandardOutput= while looking for units somewhere else entirely.
// Installing is refused instead of writing a unit that does nothing.
func TestServiceInstallRefusesRelativePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no scheduler integration on windows")
	}
	t.Setenv("HOME", "relative-home")
	t.Setenv("XDG_STATE_HOME", "")
	app := &App{Profile: "default", Paths: config.Paths{StateDir: "relative-state"}}
	err := app.serviceInstall(time.Minute)
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("install should refuse a relative path, got %v", err)
	}
}

// scanPlist walks the document the way a parser does, failing on anything
// malformed, and collects the text of every element by tag name. Nesting is
// flattened: the test cares that a value is present and intact, not where in
// the tree it sits.
func scanPlist(t *testing.T, doc string) map[string][]string {
	t.Helper()
	byTag := map[string][]string{}
	dec := xml.NewDecoder(strings.NewReader(doc))
	dec.Strict = true
	var tag string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return byTag
		}
		if err != nil {
			t.Fatalf("launchd would refuse this plist: %v\n%s", err, doc)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			tag = tok.Name.Local
			if _, ok := byTag[tag]; !ok {
				byTag[tag] = nil
			}
		case xml.CharData:
			if s := strings.TrimSpace(string(tok)); s != "" && tag != "" {
				byTag[tag] = append(byTag[tag], s)
			}
		case xml.EndElement:
			tag = ""
		}
	}
}

func TestLaunchdPlistParsesAndPinsTheInstallEnvironment(t *testing.T) {
	env := [][2]string{{"VINTED_BASE_URL", "https://example.test/?a=1&b=2"}}
	doc := launchdPlist("dev.micr.vinted-cli.default", "/usr/local/bin/vinted", checkArgs, "/Users/u/log/w.log", 10*time.Minute, env)

	p := scanPlist(t, doc)
	for _, key := range []string{"Label", "ProgramArguments", "EnvironmentVariables", "StartInterval", "RunAtLoad", "StandardOutPath", "StandardErrorPath"} {
		if !contains(p["key"], key) {
			t.Fatalf("plist is missing %s:\n%s", key, doc)
		}
	}
	if !contains(p["integer"], "600") {
		t.Fatalf("StartInterval must be the interval in seconds:\n%s", doc)
	}
	// The ampersand survives the round trip, which is the point of escaping it.
	if !contains(p["string"], "https://example.test/?a=1&b=2") {
		t.Fatalf("the environment value did not survive escaping:\n%v", p["string"])
	}
	if !strings.Contains(doc, "&amp;") {
		t.Fatalf("the raw plist must escape & rather than emit it bare:\n%s", doc)
	}
	if !contains(p["string"], "/usr/local/bin/vinted") || !contains(p["string"], "--all") {
		t.Fatalf("ProgramArguments must hold the executable and its arguments:\n%v", p["string"])
	}
}

func TestSchtasksRoundsUpToWholeMinutesAndQuotesTheExecutable(t *testing.T) {
	got := schtasksCreate("vinted-watch-default", `C:\Program Files\vinted.exe`, checkArgs, 90*time.Second)
	want := `schtasks /Create /SC MINUTE /MO 2 /TN "vinted-watch-default" /TR "\"C:\Program Files\vinted.exe\" watch check --all --profile default --format jsonl"`
	if got != want {
		t.Fatalf("schtasks line:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(schtasksCreate("t", "w.exe", nil, 30*time.Second), "/MO 1 ") {
		t.Fatal("an interval below a minute must round up to 1, since schtasks rejects 0")
	}
}

// The rendered unit must reach the same files the CLI just used, whatever the
// caller's environment. This is the failure the pinning exists to prevent: a
// timer quietly checking an empty state database in a different home.
func TestInstalledUnitReadsTheSameStateAsTheCommandThatInstalledIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir+"/data")
	t.Setenv("XDG_STATE_HOME", dir+"/state")
	t.Setenv("XDG_CONFIG_HOME", dir+"/config")
	service, _ := systemdUnits("default", "/usr/bin/vinted", checkArgs, dir+"/state/w.log", time.Minute, serviceEnv())
	for _, want := range []string{
		`Environment="XDG_DATA_HOME=` + dir + `/data"`,
		`Environment="XDG_STATE_HOME=` + dir + `/state"`,
		`Environment="XDG_CONFIG_HOME=` + dir + `/config"`,
	} {
		if !strings.Contains(service, want) {
			t.Fatalf("unit does not pin %s:\n%s", want, service)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
