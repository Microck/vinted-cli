package cli_test

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/pflag"

	"github.com/Microck/vinted-cli/internal/cli"
)

// mcpSession drives a real `vinted mcp` server over the stdio transport:
// JSON-RPC lines into its stdin, lines out of its stdout. Nothing reaches into
// the mcp package; the tests see exactly what a harness would.
type mcpSession struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *bufio.Reader
	exit chan int
	id   int
	once sync.Once
}

func (h *harness) mcpServer(args ...string) *mcpSession {
	h.t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	exit := make(chan int, 1)
	go func() {
		code := cli.Execute("test", append([]string{"mcp"}, args...), inR, outW, io.Discard)
		outW.Close()
		exit <- code
	}()
	s := &mcpSession{t: h.t, in: inW, out: bufio.NewReader(outR), exit: exit}
	h.t.Cleanup(s.close)
	return s
}

// close ends the session the way a harness does: shut stdin, expect exit 0.
func (s *mcpSession) close() {
	s.once.Do(func() {
		s.in.Close()
		select {
		case code := <-s.exit:
			if code != 0 {
				s.t.Errorf("mcp server exited %d", code)
			}
		case <-time.After(10 * time.Second):
			s.t.Error("mcp server did not exit after stdin closed")
		}
	})
}

func (s *mcpSession) write(msg map[string]any) {
	s.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(raw, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// request sends one request and returns the response object.
func (s *mcpSession) request(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	msg := map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	s.write(msg)
	line, err := s.out.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("reading response to %s: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		s.t.Fatalf("response to %s is not json: %v\n%s", method, err, line)
	}
	if resp["jsonrpc"] != "2.0" {
		s.t.Fatalf("response to %s is not jsonrpc 2.0: %s", method, line)
	}
	if id, _ := resp["id"].(float64); int(id) != s.id {
		s.t.Fatalf("response id %v, want %d: %s", resp["id"], s.id, line)
	}
	return resp
}

func (s *mcpSession) result(method string, params any) map[string]any {
	s.t.Helper()
	resp := s.request(method, params)
	if e, ok := resp["error"]; ok {
		s.t.Fatalf("%s failed: %v", method, e)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("%s returned no result object: %v", method, resp)
	}
	return res
}

// callTool returns the tool's text content and whether it reported an error.
func (s *mcpSession) callTool(name string, args map[string]any) (string, bool) {
	s.t.Helper()
	res := s.result("tools/call", map[string]any{"name": name, "arguments": args})
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		s.t.Fatalf("tool %s returned no content", name)
	}
	// A failed command answers with what it printed and then its envelope, so
	// the last block is the one that matters either way.
	block, _ := content[len(content)-1].(map[string]any)
	if block["type"] != "text" {
		s.t.Fatalf("tool %s returned a %v block", name, block["type"])
	}
	text, _ := block["text"].(string)
	isError, _ := res["isError"].(bool)
	return text, isError
}

// callToolError returns the JSON-RPC error message for a call the protocol
// itself refuses (unknown tool, malformed arguments).
func (s *mcpSession) callToolError(name string, args map[string]any) string {
	s.t.Helper()
	resp := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	e, ok := resp["error"].(map[string]any)
	if !ok {
		s.t.Fatalf("tools/call %s should have failed: %v", name, resp)
	}
	msg, _ := e["message"].(string)
	return msg
}

func (s *mcpSession) toolNames() []string {
	s.t.Helper()
	res := s.result("tools/list", nil)
	raw, _ := res["tools"].([]any)
	names := make([]string, 0, len(raw))
	for _, t := range raw {
		tool, _ := t.(map[string]any)
		name, _ := tool["name"].(string)
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			s.t.Errorf("tool %s has no object input schema: %v", name, schema)
		}
		if desc, _ := tool["description"].(string); desc == "" {
			s.t.Errorf("tool %s has no description", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestMCPHandshakeListsOnlyTheToolsThatMirrorReadCommands(t *testing.T) {
	h := newHarness(t)
	s := h.mcpServer()

	init := s.result("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocol version %v", init["protocolVersion"])
	}
	info, _ := init["serverInfo"].(map[string]any)
	if info["name"] != "vinted" || info["version"] != "test" {
		t.Errorf("serverInfo %v", info)
	}
	caps, _ := init["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("tools capability missing: %v", caps)
	}

	// A notification takes no reply, malformed or not, so the next response
	// must be the ping's.
	s.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.write(map[string]any{"jsonrpc": "2.0"})
	s.result("ping", nil)

	got := s.toolNames()
	want := []string{"alert_list", "brand_search", "category_list", "chat_list", "chat_show", "item_show", "me_favorites", "me_show", "search", "search_filters", "user_items", "user_reviews", "user_show", "watch_check"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", got, want)
	}
}

func TestMCPWithholdsActionsThatReachOtherPeople(t *testing.T) {
	h := newHarness(t)
	s := h.mcpServer()
	for _, name := range []string{"chat_send", "chat_start", "chat_open", "chat_offer", "chat_reserve", "item_favorite", "item_unfavorite", "item_create", "item_edit", "item_delete", "item_hide", "item_unhide"} {
		if msg := s.callToolError(name, nil); !strings.Contains(msg, "not offered") {
			t.Errorf("%s: %q", name, msg)
		}
	}
	if msg := s.callToolError("nonsense", nil); !strings.Contains(msg, "unknown tool") {
		t.Errorf("unknown tool message: %q", msg)
	}
	if resp := s.request("vinted/please", nil); resp["error"] == nil {
		t.Errorf("unknown method should be a jsonrpc error: %v", resp)
	}
	// A malformed envelope runs nothing: no version marker, or an id that is
	// not a scalar.
	malformed := []map[string]any{
		{"id": 99, "method": "tools/call", "params": map[string]any{"name": "watch_check"}},
		{"jsonrpc": "2.0", "id": map[string]any{"a": 1}, "method": "tools/call", "params": map[string]any{"name": "watch_check"}},
	}
	for _, msg := range malformed {
		s.write(msg)
		line, err := s.out.ReadBytes('\n')
		if err != nil {
			t.Fatalf("no answer to %v: %v", msg, err)
		}
		if !strings.Contains(string(line), "-32600") {
			t.Errorf("%v was not refused: %s", msg, line)
		}
	}
}

func TestMCPToolOutputIsTheSameJSONTheCommandPrints(t *testing.T) {
	h := newHarness(t)
	h.login()
	s := h.mcpServer()

	text, isErr := s.callTool("search", map[string]any{
		"text":      "levis 501",
		"price_to":  30,
		"condition": []any{"good"},
		"order":     "newest_first",
		"limit":     3,
	})
	if isErr {
		t.Fatalf("search tool failed: %s", text)
	}
	cmd := h.must("", "search", "levis", "501", "--price-to", "30", "--condition", "good", "--order", "newest_first", "--limit", "3")
	if text != strings.TrimRight(cmd.stdout, "\n") {
		t.Fatalf("tool output differs from the command\ntool: %s\ncmd:  %s", text, cmd.stdout)
	}
	if q := h.fake.requestsTo("/catalog")[0].Query; q.Get("search_text") != "levis 501" || q.Get("order") != "newest_first" {
		t.Fatalf("tool arguments did not reach the query: %v", q)
	}

	// Text rides behind --, so one that starts with a dash is still a search term.
	if text, isErr = s.callTool("search", map[string]any{"text": "--help"}); isErr {
		t.Fatalf("search for a flag-like text failed: %s", text)
	}
	if page := decode[itemsOut](t, text); len(page.Items) == 0 {
		t.Fatalf("search ran help instead: %s", text)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
		cmd  []string
	}{
		{"item_show", map[string]any{"item": fixtureItemRef()}, []string{"item", "show", fixtureItemRef()}},
		{"user_show", map[string]any{"user": "123456789"}, []string{"user", "show", "123456789"}},
		{"me_show", nil, []string{"me", "show"}},
		{"chat_list", map[string]any{"limit": 5}, []string{"chat", "list", "--limit", "5"}},
	} {
		text, isErr := s.callTool(tc.tool, tc.args)
		if isErr {
			t.Fatalf("%s failed: %s", tc.tool, text)
		}
		if text != strings.TrimRight(h.must("", tc.cmd...).stdout, "\n") {
			t.Fatalf("%s differs from the command:\n%s", tc.tool, text)
		}
	}
}

func TestMCPFailedCommandComesBackAsTheErrorEnvelope(t *testing.T) {
	h := newHarness(t)
	s := h.mcpServer()
	text, isErr := s.callTool("item_show", map[string]any{"item": "1"})
	if !isErr {
		t.Fatalf("missing item should be a tool error: %s", text)
	}
	env := decode[struct {
		Code     int    `json:"code"`
		Category string `json:"category"`
		Message  string `json:"message"`
	}](t, text)
	if env.Code != 4 || env.Category != "not_found" || env.Message == "" {
		t.Fatalf("envelope %+v", env)
	}
	text, isErr = s.callTool("me_show", nil)
	if env := decode[map[string]any](t, text); !isErr || env["code"] != float64(3) {
		t.Fatalf("me_show without a session: %s", text)
	}
}

func TestMCPWatchCheckToolRunsTheProfilesWatches(t *testing.T) {
	h := newHarness(t)
	h.must("", "watch", "add", "seller", "123456789", "--name", "seller")
	h.fake.addWardrobeItem(999999002)

	s := h.mcpServer()
	text, isErr := s.callTool("watch_check", map[string]any{"all": true})
	if isErr {
		t.Fatalf("watch_check failed: %s", text)
	}
	events := decode[[]eventOut](t, text)
	if len(events) != 1 || events[0].Type != "seller.new_item" || events[0].Item.ID != 999999002 {
		t.Fatalf("events %+v", events)
	}
	// The state is stored, so a second check through the server reports nothing.
	text, _ = s.callTool("watch_check", map[string]any{"names": []any{"seller"}})
	if events := decode[[]eventOut](t, text); len(events) != 0 {
		t.Fatalf("second check re-emitted: %+v", events)
	}
}

func TestMCPRejectsArgumentsTheSchemaDoesNotAllow(t *testing.T) {
	h := newHarness(t)
	s := h.mcpServer()
	if msg := s.callToolError("search", map[string]any{"colour": "red"}); !strings.Contains(msg, `unknown argument "colour"`) {
		t.Errorf("unknown argument: %q", msg)
	}
	if msg := s.callToolError("item_show", nil); !strings.Contains(msg, `"item" is required`) {
		t.Errorf("missing required: %q", msg)
	}
	if msg := s.callToolError("item_show", map[string]any{"item": 12}); !strings.Contains(msg, "must be a string") {
		t.Errorf("wrong type: %q", msg)
	}
	if msg := s.callToolError("search", map[string]any{"limit": 1.5}); !strings.Contains(msg, "whole number") {
		t.Errorf("fractional integer: %q", msg)
	}
	// An explicit null is a mistake, not an omission.
	if msg := s.callToolError("item_show", map[string]any{"item": nil}); !strings.Contains(msg, "must not be null") {
		t.Errorf("null argument: %q", msg)
	}
}

func TestMCPServerActsAsTheProfileItWasStartedWith(t *testing.T) {
	h := newHarness(t)
	h.login("--profile", "work")
	h.must("", "config", "set", "default_profile", "other")
	s := h.mcpServer("--profile", "work")
	if text, isErr := s.callTool("me_show", nil); isErr {
		t.Fatalf("me_show as the work profile failed: %s", text)
	}
	s2 := h.mcpServer()
	if text, isErr := s2.callTool("me_show", nil); !isErr {
		t.Fatalf("a profile without a session must not borrow another's: %s", text)
	}
}

// install

func TestMCPInstallIsIdempotentPerHarness(t *testing.T) {
	h := newHarness(t)
	claude := filepath.Join(h.home, ".claude.json")
	if err := os.WriteFile(claude, []byte(`{"numStartups":7,"mcpServers":{"other":{"command":"other-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(h.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	existingCodex := "# my codex config\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"other-mcp\"\n"
	if err := os.WriteFile(codex, []byte(existingCodex), 0o600); err != nil {
		t.Fatal(err)
	}

	type change struct {
		Harness string
		Path    string
		Action  string
		Command string
		Args    []string
	}
	install := func(args ...string) change {
		var c change
		decodeInto(t, h.must("", append([]string{"mcp", "install"}, args...)...).stdout, &c)
		return c
	}

	for _, tc := range []struct{ harness, path string }{
		{"claude-code", claude},
		{"codex", codex},
		{"cursor", filepath.Join(h.home, ".cursor", "mcp.json")},
	} {
		c := install(tc.harness)
		if c.Action != "added" || c.Path != tc.path {
			t.Fatalf("%s: %+v", tc.harness, c)
		}
		if c.Command == "" || strings.Join(c.Args, " ") != "mcp" {
			t.Fatalf("%s entry: %+v", tc.harness, c)
		}
		first, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if c := install(tc.harness); c.Action != "unchanged" {
			t.Fatalf("%s: second install said %q", tc.harness, c.Action)
		}
		again, _ := os.ReadFile(tc.path)
		if string(again) != string(first) {
			t.Fatalf("%s: second install rewrote the file\n%s\n%s", tc.harness, first, again)
		}
		if c := install(tc.harness, "--profile", "work"); c.Action != "updated" || strings.Join(c.Args, " ") != "mcp --profile work" {
			t.Fatalf("%s: profile install %+v", tc.harness, c)
		}
		updated, _ := os.ReadFile(tc.path)
		if !strings.Contains(string(updated), "--profile") {
			t.Fatalf("%s: profile not written\n%s", tc.harness, updated)
		}
	}

	// Entries this CLI does not own survive, comments included.
	var claudeDoc struct {
		NumStartups int `json:"numStartups"`
		MCPServers  map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(claude)
	decodeInto(t, string(raw), &claudeDoc)
	if claudeDoc.NumStartups != 7 || claudeDoc.MCPServers["other"].Command != "other-mcp" {
		t.Fatalf("claude config lost keys: %s", raw)
	}
	if _, ok := claudeDoc.MCPServers["vinted"]; !ok {
		t.Fatalf("claude config has no vinted entry: %s", raw)
	}
	codexBody, _ := os.ReadFile(codex)
	for _, want := range []string{"# my codex config", `model = "gpt-5"`, "[mcp_servers.other]", "[mcp_servers.vinted]"} {
		if !strings.Contains(string(codexBody), want) {
			t.Fatalf("codex config lost %q:\n%s", want, codexBody)
		}
	}
	if strings.Count(string(codexBody), "[mcp_servers.vinted]") != 1 {
		t.Fatalf("codex entry duplicated:\n%s", codexBody)
	}
}

func TestMCPInstallKeepsSettingsItDoesNotOwn(t *testing.T) {
	h := newHarness(t)
	// An entry can carry env, timeouts or harness-specific keys the person put
	// there; only command and args belong to this CLI.
	cursor := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	// The big integer is there on purpose: a float64 round-trip would round it.
	existing := `{"bigId":9007199254740993,"mcpServers":{"vinted":{"command":"old","args":["mcp"],"env":{"VINTED_PROFILE":"work"},"timeout":60}}}`
	if err := os.WriteFile(cursor, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	h.must("", "mcp", "install", "cursor")

	var doc struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
			Env     map[string]string
			Timeout int
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(cursor)
	decodeInto(t, string(raw), &doc)
	entry := doc.MCPServers["vinted"]
	if entry.Command == "old" || entry.Env["VINTED_PROFILE"] != "work" || entry.Timeout != 60 {
		t.Fatalf("install dropped settings it does not own: %s", raw)
	}
	if !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("a large integer was rounded on the way through: %s", raw)
	}
}

func TestMCPInstallHandlesAnEmptyJSONDocument(t *testing.T) {
	h := newHarness(t)
	cursor := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	// `null` is valid JSON and decodes to no map at all.
	if err := os.WriteFile(cursor, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.must("", "mcp", "install", "cursor")
	raw, _ := os.ReadFile(cursor)
	if !strings.Contains(string(raw), `"vinted"`) {
		t.Fatalf("entry not written over a null document: %s", raw)
	}
}

func TestMCPInstallKeepsCodexServerSettings(t *testing.T) {
	h := newHarness(t)
	codex := filepath.Join(h.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	// codex keeps its own per-server settings in the same table.
	// The next table is indented, which TOML allows: the rewrite must stop at
	// it rather than reaching into another server's settings.
	body := "[mcp_servers.vinted]\ncommand = \"old\"\n# how long codex waits\nstartup_timeout_sec = 30\nargs = [\n  \"mcp\",\n]\nenabled = true\n\n  [mcp_servers.other]\n  command = \"other-mcp\"\n"
	if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.must("", "mcp", "install", "codex")

	after, _ := os.ReadFile(codex)
	for _, want := range []string{"# how long codex waits", "startup_timeout_sec = 30", "enabled = true", "[mcp_servers.other]", "command = \"other-mcp\""} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("install dropped %q:\n%s", want, after)
		}
	}
	if strings.Contains(string(after), `command = "old"`) || strings.Contains(string(after), "\"mcp\",\n") {
		t.Fatalf("the old command or args survived:\n%s", after)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(after, &doc); err != nil {
		t.Fatalf("install left invalid toml: %v\n%s", err, after)
	}
	if doc.MCPServers["vinted"].Command == "old" || strings.Join(doc.MCPServers["vinted"].Args, " ") != "mcp" {
		t.Fatalf("entry not updated: %+v", doc.MCPServers["vinted"])
	}
}

func TestMCPInstallUpdatesDecodedCodexKeys(t *testing.T) {
	for _, assignments := range []string{
		`"command" = "/opt/we[ird/vinted"
'args' = ["old"] # bracket in comment [
`,
		`"\u0063ommand" = "old"
"\U00000061rgs" = [
  """
[profile]
""",
]
`,
	} {
		t.Run(assignments, func(t *testing.T) {
			h := newHarness(t)
			codex := filepath.Join(h.home, ".codex", "config.toml")
			if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
				t.Fatal(err)
			}
			kept := "# keep these settings\nenabled = true\nnote = \"\"\"\ncommand = literal\n\\\"args\\\" = text\n\"\"\"\n"
			other := "\n[mcp_servers.other]\ncommand = \"other-mcp\"\n"
			body := "[mcp_servers.vinted]\n" + kept + assignments + other
			if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			var change struct {
				Action  string
				Command string
			}
			decodeInto(t, h.must("", "mcp", "install", "codex").stdout, &change)
			after, err := os.ReadFile(codex)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				MCPServers map[string]struct {
					Command string
					Args    []string
				} `toml:"mcp_servers"`
			}
			if err := toml.Unmarshal(after, &doc); err != nil {
				t.Fatalf("install left duplicate or invalid keys: %v\n%s", err, after)
			}
			entry := doc.MCPServers["vinted"]
			if change.Action != "updated" || entry.Command != change.Command || strings.Join(entry.Args, " ") != "mcp" {
				t.Fatalf("entry not updated: %+v, change %+v", entry, change)
			}
			if !strings.Contains(string(after), kept) || !strings.Contains(string(after), other) {
				t.Fatalf("install changed unowned settings or comments:\n%s", after)
			}
			decodeInto(t, h.must("", "mcp", "install", "codex").stdout, &change)
			again, err := os.ReadFile(codex)
			if err != nil || change.Action != "unchanged" || string(again) != string(after) {
				t.Fatalf("second install mutated the config: action %q, error %v\n%s", change.Action, err, again)
			}
		})
	}
}

func TestMCPInstallRefusesTableValuedCodexKeys(t *testing.T) {
	for _, owned := range []string{
		"[mcp_servers.vinted.command]\npath = \"old\"\n",
		"[mcp_servers.vinted.\"\\u0061rgs\"]\nextra = 1\n",
		"[[mcp_servers.vinted.args]]\nextra = 1\n",
		"command.path = \"old\"\nargs.extra = 1\n",
		"command = {path = \"old\"}\n",
	} {
		t.Run(owned, func(t *testing.T) {
			h := newHarness(t)
			codex := filepath.Join(h.home, ".codex", "config.toml")
			if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
				t.Fatal(err)
			}
			body := "# keep this file intact\n[mcp_servers.vinted]\nenabled = true\n" + owned
			if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			r := h.run("", "mcp", "install", "codex")
			if r.code == 0 {
				t.Fatalf("install accepted a table-valued owned key: %s", r.stdout)
			}
			after, err := os.ReadFile(codex)
			if err != nil || string(after) != body {
				t.Fatalf("install mutated the config on failure: %v\n%s", err, after)
			}
		})
	}
}

func TestMCPInstallFollowsASymlinkedConfig(t *testing.T) {
	h := newHarness(t)
	real := filepath.Join(h.home, "dotfiles", "cursor-mcp.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h.must("", "mcp", "install", "cursor")

	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced the symlink with a file: %v", fi.Mode())
	}
	raw, _ := os.ReadFile(real)
	if !strings.Contains(string(raw), `"vinted"`) {
		t.Fatalf("the symlink target was not updated: %s", raw)
	}

	// A link whose target does not exist yet is still where the file belongs,
	// through a chain of them as a dotfile manager tends to leave.
	dangling := filepath.Join(h.home, ".claude.json")
	middle := filepath.Join(h.home, "dotfiles", "claude-link.json")
	wanted := filepath.Join(h.home, "dotfiles", "claude.json")
	if err := os.Symlink(middle, dangling); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wanted, middle); err != nil {
		t.Fatal(err)
	}
	h.must("", "mcp", "install", "claude-code")
	if fi, err := os.Lstat(dangling); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("install replaced the dangling symlink: %v", fi.Mode())
	}
	if raw, err := os.ReadFile(wanted); err != nil || !strings.Contains(string(raw), `"vinted"`) {
		t.Fatalf("the dangling link's target was not written: %v %s", err, raw)
	}
}

func TestMCPInstallUpdatesACodexConfigWithWindowsLineEndings(t *testing.T) {
	h := newHarness(t)
	codex := filepath.Join(h.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[mcp_servers.vinted]\r\ncommand = \"old\"\r\nargs = [\"mcp\"]\r\nenabled = true\r\n"
	if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var c struct{ Action string }
	decodeInto(t, h.must("", "mcp", "install", "codex").stdout, &c)
	if c.Action != "updated" {
		t.Fatalf("action %q, want updated", c.Action)
	}
	after, _ := os.ReadFile(codex)
	if strings.Contains(string(after), `command = "old"`) || !strings.Contains(string(after), "enabled = true") {
		t.Fatalf("crlf config not updated in place:\n%q", after)
	}
}

func TestMCPInstallRefusesJSONWithDataAfterTheDocument(t *testing.T) {
	h := newHarness(t)
	cursor := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	// Rewriting this file would drop the second document silently.
	if err := os.WriteFile(cursor, []byte("{\"mcpServers\":{}}\n{\"another\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "mcp", "install", "cursor")
	if r.code == 0 || !strings.Contains(r.stderr, "not valid json") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

func TestMCPInstallRefusesAConfigWhereTheEntryIsNotATable(t *testing.T) {
	h := newHarness(t)
	cursor := filepath.Join(h.home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursor, []byte(`{"mcpServers":{"vinted":"not a table"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "mcp", "install", "cursor"); r.code == 0 || !strings.Contains(r.stderr, "not a table") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}

	codex := filepath.Join(h.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "mcp_servers = \"nonsense\"\n"
	if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "mcp", "install", "codex"); r.code == 0 || !strings.Contains(r.stderr, "not a table") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	if after, _ := os.ReadFile(codex); string(after) != body {
		t.Fatalf("the config was written anyway:\n%s", after)
	}
}

func TestMCPInstallRefusesATomlEntryItCannotRewrite(t *testing.T) {
	h := newHarness(t)
	codex := filepath.Join(h.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	// A spelling the parser accepts but the rewrite does not recognise:
	// appending a second table would leave the file invalid.
	body := "[\"mcp_servers\".\"vinted\"]\ncommand = \"old\"\nargs = [\"mcp\"]\n"
	if err := os.WriteFile(codex, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "mcp", "install", "codex")
	if r.code == 0 || !strings.Contains(r.stderr, "by hand") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	after, _ := os.ReadFile(codex)
	if string(after) != body {
		t.Fatalf("the config was touched anyway:\n%s", after)
	}
}

func TestMCPInstallRejectsAnUnknownHarness(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "mcp", "install", "emacs")
	if r.code != 2 || !strings.Contains(r.stderr, "claude-code") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

// The search tools take one argument per search flag. A flag added to the
// command without its argument would be out of reach for an agent, so the
// two are compared here rather than by eye.
func TestMCPSearchToolsMirrorEverySearchFlag(t *testing.T) {
	h := newHarness(t)
	s := h.mcpServer()
	res := s.result("tools/list", nil)
	schemas := map[string]map[string]any{}
	for _, raw := range res["tools"].([]any) {
		tool := raw.(map[string]any)
		props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		schemas[tool["name"].(string)] = props
	}
	root := cli.CommandTree("test")
	for tool, path := range map[string][]string{"search": {"search"}, "search_filters": {"search", "filters"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" {
				return
			}
			if _, ok := schemas[tool][strings.ReplaceAll(f.Name, "-", "_")]; !ok {
				t.Errorf("%s: flag --%s has no tool argument", tool, f.Name)
			}
		})
	}
}

// A bare install follows the config's default profile wherever it points
// later; only an explicit choice pins one.
func TestMCPInstallPinsOnlyAnExplicitProfile(t *testing.T) {
	h := newHarness(t)
	h.must("", "config", "set", "default_profile", "other")
	var c struct{ Args []string }
	decodeInto(t, h.must("", "mcp", "install", "cursor").stdout, &c)
	if strings.Join(c.Args, " ") != "mcp" {
		t.Fatalf("a bare install pinned the current default: %v", c.Args)
	}
	t.Setenv("VINTED_PROFILE", "work")
	decodeInto(t, h.must("", "mcp", "install", "cursor").stdout, &c)
	if strings.Join(c.Args, " ") != "mcp --profile work" {
		t.Fatalf("VINTED_PROFILE was not pinned: %v", c.Args)
	}
}
