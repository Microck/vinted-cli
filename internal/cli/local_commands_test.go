package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover the commands that never talk to Vinted (profiles, config,
// sinks, completion, the local watch views) and the CLI's own input errors.

func (f *fakeVinted) setItemPage(page []byte) {
	f.mu.Lock()
	f.itemPage = page
	f.mu.Unlock()
}

func TestProfileListUseAndRemove(t *testing.T) {
	h := newHarness(t)
	h.login("--profile", "work")
	h.login("--profile", "home")
	h.must("", "watch", "add", "seller", "123456789", "--name", "home-seller", "--profile", "home")

	type row struct {
		Name    string `json:"name"`
		Account string `json:"account"`
		Default bool   `json:"default"`
	}
	list := decode[[]row](t, h.must("", "profile", "list").stdout)
	if len(list) != 2 || list[0].Name != "home" || list[1].Name != "work" || !list[1].Default || list[0].Default || list[0].Account != fixtureLogin {
		t.Fatalf("the first login should be the default: %+v", list)
	}
	if pretty := h.must("", "profile", "list", "--format", "pretty").stdout; !strings.Contains(pretty, "PROFILE") || !strings.Contains(pretty, "*  work") {
		t.Fatalf("pretty profile list:\n%s", pretty)
	}

	if r := h.run("", "profile", "use", "nobody"); r.code != 2 || !strings.Contains(r.stderr, "auth login --profile nobody") {
		t.Fatalf("use without a session: exit %d %q", r.code, r.stderr)
	}
	h.must("", "profile", "use", "home")
	if v := strings.TrimSpace(h.must("", "config", "get", "default_profile").stdout); v != `"home"` {
		t.Fatalf("default_profile %s", v)
	}

	// Removing asks; without a terminal the answer has to be --yes.
	if r := h.run("", "profile", "remove", "home"); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("remove without --yes: exit %d %q", r.code, r.stderr)
	}
	if len(decode[[]row](t, h.must("", "profile", "list").stdout)) != 2 {
		t.Fatal("a refused remove removed the profile")
	}
	h.must("", "profile", "remove", "home", "--yes")
	list = decode[[]row](t, h.must("", "profile", "list").stdout)
	if len(list) != 1 || list[0].Name != "work" {
		t.Fatalf("after remove: %+v", list)
	}
	if r := h.run("", "config", "get", "default_profile"); r.code != 2 {
		t.Fatalf("removing the default profile must clear default_profile: %s", r.stdout)
	}
	if ws := decode[[]map[string]any](t, h.must("", "watch", "list", "--all-profiles").stdout); len(ws) != 0 {
		t.Fatalf("the removed profile's watches survived: %v", ws)
	}
}

func TestConfigPathListAndRejectedSets(t *testing.T) {
	h := newHarness(t)
	paths := decode[map[string]string](t, h.must("", "config", "path").stdout)
	for key, want := range map[string]string{
		"config":      filepath.Join(h.home, "config", "vinted-cli", "config.toml"),
		"credentials": h.credentialsFile(),
		"state":       filepath.Join(h.home, "data", "vinted-cli", "state.db"),
	} {
		if paths[key] != want {
			t.Errorf("config path %s = %q, want %q", key, paths[key], want)
		}
	}

	h.must("", "config", "set", "watch.interval", "30m")
	h.must("", "config", "set", "request_interval", "3s")
	h.must("", "config", "set", "proxy", "socks5h://127.0.0.1:1080")
	h.must("", "config", "set", "sinks.script.type", "exec")
	h.must("", "config", "set", "sinks.script.command", `["/bin/true", "--flag"]`)
	cfg := decode[map[string]any](t, h.must("", "config", "list").stdout)
	if cfg["request_interval"] != "3s" || cfg["proxy"] != "socks5h://127.0.0.1:1080" || cfg["watch"].(map[string]any)["interval"] != "30m" {
		t.Fatalf("config list %v", cfg)
	}
	if cmd := cfg["sinks"].(map[string]any)["script"].(map[string]any)["command"]; len(cmd.([]any)) != 2 {
		t.Fatalf("exec command not stored as an array: %v", cmd)
	}
	before, err := os.ReadFile(paths["config"])
	if err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"watch.interval", "1m"},                 // under the 15m floor
		{"watch.interval", "soon"},               // not a duration
		{"request_interval", "500ms"},            // under the 1s floor
		{"proxy", "http://user:pass@proxy:8080"}, // credentials in a world-readable file
		{"proxy", "ftp://proxy"},                 // unsupported scheme
		{"sinks.phone.type", "pigeon"},           // unknown sink type
		{"sinks.hook.template", "teams"},         // unknown webhook template
		{"sinks.script.command", "/bin/true"},    // not an array
		{"profiles.default.location", "Madrid"},  // no such key
	} {
		if r := h.run("", append([]string{"config", "set"}, args...)...); r.code != 2 {
			t.Errorf("config set %v: exit %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}
	after, _ := os.ReadFile(paths["config"])
	if string(after) != string(before) {
		t.Fatalf("a rejected set changed config.toml:\n%s", after)
	}
	if r := h.run("", "config", "get", "sinks.phone"); r.code != 2 {
		t.Fatalf("get of a missing key: exit %d", r.code)
	}
	// A sink is set up one key at a time: a url before the type is fine.
	h.must("", "config", "set", "sinks.phone.url", "https://ntfy.example")
}

// sinkCapture is a local ntfy/webhook endpoint that records what it was sent.
type sinkCapture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
}

type capturedRequest struct {
	path   string
	header http.Header
	body   string
}

func newSinkCapture(t *testing.T) *sinkCapture {
	c := &sinkCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(raw)
		c.mu.Lock()
		c.reqs = append(c.reqs, capturedRequest{path: r.URL.Path, header: r.Header.Clone(), body: string(raw)})
		c.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/fail") {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *sinkCapture) last(t *testing.T) capturedRequest {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("the sink received nothing")
	}
	return c.reqs[len(c.reqs)-1]
}

func TestSinkListAndTestDeliverToEveryType(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the exec sink fixture is a shell script")
	}
	h := newHarness(t)
	capture := newSinkCapture(t)
	script := filepath.Join(h.home, "on-event.sh")
	out := filepath.Join(h.home, "exec-out.txt")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n{ echo \"$1 $VINTED_EVENT_TYPE $VINTED_WATCH\"; cat; } > "+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"sinks.phone.type", "ntfy"}, {"sinks.phone.url", capture.srv.URL + "/"}, {"sinks.phone.topic", "deals"},
		{"sinks.chat.type", "webhook"}, {"sinks.chat.url", capture.srv.URL + "/chat"}, {"sinks.chat.template", "discord"},
		{"sinks.raw.type", "webhook"}, {"sinks.raw.url", capture.srv.URL + "/raw"},
		{"sinks.down.type", "webhook"}, {"sinks.down.url", capture.srv.URL + "/fail"},
		{"sinks.script.type", "exec"}, {"sinks.script.command", `["` + script + `", "--verbose"]`},
		{"sinks.half.type", "ntfy"},
	} {
		h.must("", "config", "set", kv[0], kv[1])
	}

	type row struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	list := decode[[]row](t, h.must("", "sink", "list").stdout)
	var names []string
	for _, r := range list {
		names = append(names, r.Name)
		if (r.Name == "half") != (r.Error != "") {
			t.Errorf("sink %s: problem %q", r.Name, r.Error)
		}
	}
	if strings.Join(names, ",") != "chat,down,half,phone,raw,script" {
		t.Fatalf("sink list is not sorted by name: %v", names)
	}
	if pretty := h.must("", "sink", "list", "--format", "pretty").stdout; !strings.Contains(pretty, "ntfy needs url and topic") {
		t.Fatalf("pretty sink list hides the problem:\n%s", pretty)
	}

	h.must("", "sink", "test", "phone")
	if r := capture.last(t); r.path != "/deals" || r.header.Get("Title") != "New: vinted-cli sink test" || !strings.Contains(r.body, "42 EUR") || r.header.Get("Click") == "" {
		t.Fatalf("ntfy request %+v", r)
	}
	h.must("", "sink", "test", "chat")
	if r := capture.last(t); r.path != "/chat" || !strings.Contains(r.body, `"content":"New: vinted-cli sink test`) {
		t.Fatalf("discord webhook %+v", r)
	}
	h.must("", "sink", "test", "raw")
	var ev map[string]any
	if r := capture.last(t); r.path != "/raw" || json.Unmarshal([]byte(r.body), &ev) != nil || ev["type"] != "item.new" || ev["watch"] != "sink-test" {
		t.Fatalf("raw webhook %+v", r)
	}
	h.must("", "sink", "test", "script")
	got, err := os.ReadFile(out)
	if err != nil || !strings.HasPrefix(string(got), "--verbose item.new sink-test\n") || !strings.Contains(string(got), `"title":"vinted-cli sink test"`) {
		t.Fatalf("exec sink got %q (%v)", got, err)
	}

	if r := h.run("", "sink", "test", "down"); r.code != 1 || !strings.Contains(r.stderr, "HTTP 500") {
		t.Fatalf("failing sink: exit %d %q", r.code, r.stderr)
	}
	if r := h.run("", "sink", "test", "half"); r.code != 2 {
		t.Fatalf("incomplete sink: exit %d", r.code)
	}
	if r := h.run("", "sink", "test", "nope"); r.code != 2 {
		t.Fatalf("unknown sink: exit %d", r.code)
	}
	if h.fake.requestCount() != 0 {
		t.Fatal("sink commands must not talk to vinted")
	}
}

func TestWatchEventsFiltersAndListScopes(t *testing.T) {
	h := newHarness(t)
	h.must("", "watch", "add", "seller", "123456789", "--name", "mine")
	h.must("", "watch", "add", "seller", "123456789", "--name", "theirs", "--profile", "other")
	first, _ := wardrobeIDs(t, h)
	h.fake.setWardrobeItem(first, wardrobePriceField, wardrobePrice(1.5))
	h.fake.setWardrobeItem(first, "is_reserved", true)
	if evs := checkEvents(t, h); len(evs) != 2 {
		t.Fatalf("want a price change and a reservation, got %v", evs)
	}

	if evs := decode[[]eventOut](t, h.must("", "watch", "events", "mine").stdout); len(evs) != 2 {
		t.Fatalf("events mine: %v", evs)
	}
	if evs := decode[[]eventOut](t, h.must("", "watch", "events", "--limit", "1").stdout); len(evs) != 1 {
		t.Fatalf("--limit 1: %v", evs)
	}
	if evs := decode[[]eventOut](t, h.must("", "watch", "events", "--since", "1h").stdout); len(evs) != 2 {
		t.Fatalf("--since 1h: %v", evs)
	}
	if evs := decode[[]eventOut](t, h.must("", "watch", "events", "theirs").stdout); len(evs) != 0 {
		t.Fatalf("another profile's events leaked: %v", evs)
	}
	jsonl := strings.Split(strings.TrimSpace(h.must("", "watch", "events", "--format", "jsonl").stdout), "\n")
	if len(jsonl) != 2 {
		t.Fatalf("jsonl events, one per line: %q", jsonl)
	}
	if pretty := h.must("", "watch", "events", "--format", "pretty").stdout; !strings.Contains(pretty, "item.price_changed") || !strings.Contains(pretty, "WATCH") {
		t.Fatalf("pretty events:\n%s", pretty)
	}

	if ws := decode[[]map[string]any](t, h.must("", "watch", "list").stdout); len(ws) != 1 || ws[0]["name"] != "mine" {
		t.Fatalf("watch list shows only the active profile: %v", ws)
	}
	if ws := decode[[]map[string]any](t, h.must("", "watch", "list", "--all-profiles").stdout); len(ws) != 2 {
		t.Fatalf("watch list --all-profiles: %v", ws)
	}
	if r := h.run("", "watch", "remove", "mine"); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("remove without --yes: exit %d %q", r.code, r.stderr)
	}
	h.must("", "watch", "remove", "mine", "--yes")
	if evs := decode[[]eventOut](t, h.must("", "watch", "events").stdout); len(evs) != 0 {
		t.Fatalf("events outlived their watch: %v", evs)
	}
}

func TestWatchAddRejectsBadInputBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"watch", "add", "seller", "123456789"},                                    // no --name
		{"watch", "add", "seller", "123456789", "--name", "has space"},             // bad name
		{"watch", "add", "seller", "123456789", "--name", "s", "--interval", "1m"}, // under the floor
		{"watch", "add", "seller", "123456789", "--name", "s", "--notify", "nope"}, // unknown sink
		{"watch", "add", "search", "x", "--name", "s", "--pages", "9"},             // over the page cap
		{"watch", "check", "--jitter", "-1s"},
		{"watch", "run", "--interval", "1m"},
		{"watch", "service", "install", "--interval", "1m"},
	} {
		if r := h.run("", args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}
	if n := h.fake.requestCount(); n != 0 {
		t.Fatalf("invalid input sent %d requests", n)
	}
}

// A watch whose first check fails was never created: the command reported a
// failure, and a retry must not trip over a half-made watch.
func TestFailedBaselineLeavesNoWatch(t *testing.T) {
	h := newHarness(t)
	h.fake.setBlock("/api/v2/wardrobe/", &fakeBlock{status: http.StatusNotFound, body: []byte(`{"code":404,"message":"Not found"}`)})
	if r := h.run("", "watch", "add", "seller", "123456789", "--name", "s"); r.code == 0 {
		t.Fatal("watch add succeeded against a failing wardrobe")
	}
	if ws := decode[[]map[string]any](t, h.must("", "watch", "list").stdout); len(ws) != 0 {
		t.Fatalf("a failed add left a watch behind: %v", ws)
	}
	h.fake.setBlock("", nil)
	added := decode[map[string]any](t, h.must("", "watch", "add", "seller", "123456789", "--name", "s").stdout)
	if added["baselined"] != true || added["last_check_at"] == nil {
		t.Fatalf("watch add should print the baselined watch with its check time: %v", added)
	}
}

func TestWatchCheckJitterWaitsOnlyWhenThereIsWork(t *testing.T) {
	h := newHarness(t)
	start := h.clock.Now()
	h.must("", "watch", "check", "--jitter", "10m")
	if waited := h.clock.Now().Sub(start); waited != 0 {
		t.Fatalf("no watches, yet the check slept %s", waited)
	}
	h.must("", "watch", "add", "seller", "123456789", "--name", "s")
	start = h.clock.Now()
	h.must("", "watch", "check", "--all", "--jitter", "10m")
	if waited := h.clock.Now().Sub(start); waited > 10*time.Minute+time.Minute {
		t.Fatalf("jitter of at most 10m slept %s", waited)
	}
}

func TestItemWatchReportsReservedAndSold(t *testing.T) {
	h := newHarness(t)
	h.must("", "watch", "add", "item", fixtureItemRef(), "--name", "one")
	if out := strings.TrimSpace(h.must("", "watch", "check", "one", "--format", "jsonl").stdout); out != "" {
		t.Fatalf("unchanged item produced events: %s", out)
	}
	page := fixture(t, "item.html")
	reserved := strings.ReplaceAll(string(page), `is_reserved\":false`, `is_reserved\":true`)
	if reserved == string(page) {
		t.Fatal("item fixture has no is_reserved flag to flip")
	}
	h.fake.setItemPage([]byte(reserved))
	if evs := checkEventsFor(t, h, "one"); len(evs) != 1 || evs[0].Type != "item.reserved" {
		t.Fatalf("reserving the item: %v", evs)
	}
	h.fake.setItemPage(fixture(t, "item_sold.html"))
	evs := checkEventsFor(t, h, "one")
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	if !strings.Contains(strings.Join(types, ","), "item.sold") {
		t.Fatalf("selling the item: %v", types)
	}
}

func checkEventsFor(t *testing.T, h *harness, name string) []eventOut {
	t.Helper()
	var evs []eventOut
	for _, line := range strings.Split(strings.TrimSpace(h.must("", "watch", "check", name, "--format", "jsonl").stdout), "\n") {
		if line == "" {
			continue
		}
		var ev eventOut
		decodeInto(t, line, &ev)
		evs = append(evs, ev)
	}
	return evs
}

func TestCobraInputErrorsAreUsageErrors(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"serch"}, "Did you mean search?"},
		{[]string{"watch", "lsit"}, "Did you mean list?"},
		{[]string{"watch", "bogus"}, `unknown command "bogus" for "vinted watch"`},
		{[]string{"item", "show"}, "accepts 1 arg(s), received 0"},
		{[]string{"skills", "get", "a", "b"}, "accepts 1 arg(s), received 2"},
		{[]string{"config", "path", "extra"}, "unknown command"},
		{[]string{"auth", "login", "--cookies", "x", "--cookies-stdin"}, "exclusive"},
		{[]string{"auth", "login", "--cookies", filepath.Join(h.home, "missing.txt")}, "cannot read the cookie export"},
		{[]string{"auth", "login", "--cookies", h.home}, "is a directory"},
		{[]string{"auth", "login"}, "prompts need a terminal"},
	} {
		r := h.run("", tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
			t.Errorf("%v: exit %d %q, want 2 and %q", tc.args, r.code, r.stderr, tc.want)
		}
	}
	env := decode[map[string]any](t, h.run("", "--error-format", "json", "watch", "lsit").stderr)
	if env["code"] != float64(2) || env["category"] != "usage" {
		t.Fatalf("envelope %v", env)
	}
	// A noun on its own still prints its help, and that help does not offer
	// the noun as something to run with flags.
	r := h.must("", "watch")
	if !strings.Contains(r.stdout, "vinted watch [command]") || strings.Contains(r.stdout, "vinted watch [flags]") {
		t.Fatalf("group help:\n%s", r.stdout)
	}
	if v := strings.TrimSpace(h.must("", "--version").stdout); v != "vinted test" {
		t.Fatalf("--version printed %q", v)
	}
}

func TestLoginExpandsHomeAndRecordsDomain(t *testing.T) {
	h := newHarness(t)
	raw, err := os.ReadFile(h.fake.cookieFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, "cookies.txt"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h.must("", "auth", "login", "--cookies", "~/cookies.txt", "--domain", "vinted.es")
	if v := strings.TrimSpace(h.must("", "config", "get", "profiles.default.domain").stdout); v != `"www.vinted.es"` {
		t.Fatalf("--domain vinted.es stored as %s", v)
	}
}

func TestCompletionForEveryShell(t *testing.T) {
	h := newHarness(t)
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		r := h.must("", "completion", shell)
		if len(r.stdout) < 1000 || !strings.Contains(r.stdout, "vinted") {
			t.Errorf("%s completion script is %d bytes", shell, len(r.stdout))
		}
	}
	// What every script calls back into: flag values complete from the
	// fixed sets, without touching the network.
	r := h.must("", "__complete", "search", "--order", "")
	for _, want := range []string{"relevance", "newest_first", "price_low_to_high", "price_high_to_low"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("--order completion misses %s:\n%s", want, r.stdout)
		}
	}
	if r := h.must("", "__complete", "--format", ""); !strings.Contains(r.stdout, "toon") {
		t.Errorf("--format completion:\n%s", r.stdout)
	}
	if r := h.must("", "__complete", "watch", "s"); !strings.Contains(r.stdout, "service") {
		t.Errorf("subcommand completion:\n%s", r.stdout)
	}
	if h.fake.requestCount() != 0 {
		t.Fatal("completion talked to vinted")
	}
}

func TestChatListUnreadFilters(t *testing.T) {
	h := newHarness(t)
	h.login()
	all := decode[[]map[string]any](t, h.must("", "chat", "list").stdout)
	unread := decode[[]map[string]any](t, h.must("", "chat", "list", "--unread").stdout)
	if len(unread) == 0 || len(unread) >= len(all) {
		t.Fatalf("--unread kept %d of %d", len(unread), len(all))
	}
	for _, c := range unread {
		if c["unread"] != true {
			t.Fatalf("--unread returned a read conversation: %v", c)
		}
	}
}
