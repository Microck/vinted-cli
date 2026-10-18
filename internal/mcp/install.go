package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ServerName is the key the entry is written under in every harness config.
const ServerName = "vinted"

// Harnesses are the agent harnesses `vinted mcp install` knows, in the order
// they are listed in help.
var Harnesses = []string{"claude-code", "codex", "cursor"}

// Change is what one install did, printed so the person can see the file and
// the decision rather than trusting a silent write.
type Change struct {
	Harness string   `json:"harness"`
	Path    string   `json:"path"`
	Action  string   `json:"action"` // added, updated or unchanged
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

const (
	actionAdded     = "added"
	actionUpdated   = "updated"
	actionUnchanged = "unchanged"
)

// Install writes the stdio server entry into the harness's config, leaving
// every other entry (and, in TOML, every comment) alone. Running it twice
// reports "unchanged" the second time.
func Install(harness, command string, args []string) (Change, error) {
	path, err := configPath(harness)
	if err != nil {
		return Change{}, err
	}
	ch := Change{Harness: harness, Path: path, Command: command, Args: args}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ch, err
	}
	if harness == "codex" {
		ch.Action, err = installTOML(path, command, args)
	} else {
		ch.Action, err = installJSON(path, command, args)
	}
	return ch, err
}

// configPath locates the harness config. Each harness's own home variable wins,
// so an install lands where that harness actually reads from.
func configPath(harness string) (string, error) {
	// The home directory is only resolved where it is actually needed: a
	// container or service account with CODEX_HOME set but no HOME still has
	// everything this install requires.
	inHome := func(parts ...string) (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(append([]string{home}, parts...)...), nil
	}
	switch harness {
	case "claude-code":
		if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
			return filepath.Join(dir, ".claude.json"), nil
		}
		return inHome(".claude.json")
	case "codex":
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return filepath.Join(dir, "config.toml"), nil
		}
		return inHome(".codex", "config.toml")
	case "cursor":
		return inHome(".cursor", "mcp.json")
	}
	return "", fmt.Errorf("unknown harness %q. Choose one of %s", harness, strings.Join(Harnesses, ", "))
}

// installJSON edits a harness config that is one JSON document. The file is
// decoded into a generic map and re-encoded, so keys this CLI knows nothing
// about survive; JSON carries no comments to lose.
func installJSON(path, command string, args []string) (string, error) {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if len(raw) > 0 {
		// UseNumber: a float64 round-trip would round any integer setting
		// beyond 2^53 and quietly change a value this install promised to keep.
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return "", fmt.Errorf("%s is not valid json: %w", path, err)
		}
		// Decode stops after the first value; anything following it would be
		// rewritten away, so refuse the file rather than eat half of it.
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s is not valid json: data after the document", path)
		}
		// A document of literal `null` decodes to a nil map.
		if doc == nil {
			doc = map[string]any{}
		}
	}
	servers, err := table(doc, "mcpServers", path)
	if err != nil {
		return "", err
	}
	if servers == nil {
		servers = map[string]any{}
	}
	// Only command and args are this CLI's to write: an entry may also carry
	// env, timeouts or harness-specific keys the person put there, and an
	// install must not throw them away.
	entry, err := table(servers, ServerName, path)
	if err != nil {
		return "", err
	}
	action := actionAdded
	if entry != nil {
		if entry["command"] == command && reflect.DeepEqual(entry["args"], toAny(args)) {
			return actionUnchanged, nil
		}
		action = actionUpdated
	} else {
		entry = map[string]any{}
	}
	entry["command"] = command
	entry["args"] = toAny(args)
	servers[ServerName] = entry
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	// 0600: a harness config holds the account's other server credentials.
	return action, writeFile(path, append(out, '\n'), 0o600)
}

// table returns the map at key, nil when the key is absent, and an error when
// it holds anything else. A value this CLI did not write is never overwritten,
// and writing an entry next to it would only produce a file the harness cannot
// read.
func table(doc map[string]any, key, path string) (map[string]any, error) {
	v, ok := doc[key]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s has %q set to a %T, not a table of servers. Fix it by hand and run this again", path, key, v)
	}
	return m, nil
}

// tomlTable matches the table header this CLI owns, bare or quoted;
// tomlNextTable finds where that table ends, at the next header. TOML allows
// whitespace before a header, so both accept it.
var (
	tomlTable     = regexp.MustCompile(`(?m)^[ \t]*\[mcp_servers\.(?:` + ServerName + `|"` + ServerName + `")\][ \t]*$`)
	tomlNextTable = regexp.MustCompile(`(?m)^[ \t]*\[`)
)

// installTOML edits codex's config.toml as text. A generic parse-and-remarshal
// would drop the user's comments and reorder their file, so only the lines of
// this one table are touched.
func installTOML(path, command string, args []string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	// The rewrite works line by line, so a file written on Windows is handled
	// with plain newlines and converted back on the way out; every line in it
	// had CRLF to begin with, so the file stays consistent.
	body := string(raw)
	crlf := strings.Contains(body, "\r\n")
	if crlf {
		body = strings.ReplaceAll(body, "\r\n", "\n")
	}
	// Detect a table-valued command or args from the parsed document, so
	// explicit child tables (`[mcp_servers.vinted.command]`) and dotted or
	// inline spellings are refused before any text is rewritten.
	tableValued := func(entry map[string]any) (string, bool) {
		for _, key := range []string{"command", "args"} {
			switch entry[key].(type) {
			case map[string]any:
				return key, true
			case []any:
				for _, element := range entry[key].([]any) {
					if _, ok := element.(map[string]any); ok {
						return key, true
					}
				}
			}
		}
		return "", false
	}
	entryExists := false
	entry := map[string]any(nil)
	if strings.TrimSpace(body) != "" {
		var doc map[string]any
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return "", fmt.Errorf("%s is not valid toml: %w", path, err)
		}
		servers, err := table(doc, "mcp_servers", path)
		if err != nil {
			return "", err
		}
		entry, err = table(servers, ServerName, path)
		if err != nil {
			return "", err
		}
		if entry != nil {
			entryExists = true
			if key, tableValued := tableValued(entry); tableValued {
				return "", fmt.Errorf("%s has a table-valued mcp_servers.%s.%s that this command will not rewrite. Edit it by hand: command = %s, args = [%s]",
					path, ServerName, key, strconv.Quote(command), strings.Join(quoteAll(args), ", "))
			}
			if entry["command"] == command && reflect.DeepEqual(entry["args"], toAny(args)) {
				return actionUnchanged, nil
			}
		}
	}

	block := tomlBlock(command, args)
	loc := findOwnedTable(body)
	// TOML spells one table many ways (["mcp_servers"."vinted"], an inline
	// table, quoted keys). The parse sees them all, this rewrite only sees the
	// plain header; appending a second table on top of one of the others would
	// leave the file invalid, so say so instead.
	if loc == nil && entryExists {
		return "", fmt.Errorf("%s already has an mcp_servers.%s entry in a spelling this command will not rewrite. Edit it by hand: command = %s, args = [%s]",
			path, ServerName, strconv.Quote(command), strings.Join(quoteAll(args), ", "))
	}
	action, out := actionAdded, ""
	if loc == nil {
		joined := strings.TrimRight(body, "\n")
		if joined != "" {
			joined += "\n\n"
		}
		out = joined + block
	} else {
		// Rewrite from the table header to the next top-level header. Only the
		// command and args lines are this CLI's: codex keeps its own per-server
		// settings (startup_timeout_sec, enabled, tool filters) in the same
		// table, and they stay, comments included.
		end := tableEnd(body, loc[0], loc[1])
		kept, err := stripOwnedKeys(body[loc[1]:end])
		if err != nil {
			return "", fmt.Errorf("cannot safely rewrite %s: %w", path, err)
		}
		kept = strings.TrimLeft(kept, "\n")
		action, out = actionUpdated, body[:loc[0]]+block+kept+body[end:]
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	if !parsesTOML(out) {
		return "", fmt.Errorf("cannot safely rewrite %s as valid toml. Edit it by hand and run this again", path)
	}
	return action, writeFile(path, []byte(out), 0o600)
}

// findOwnedTable locates this CLI's table header, or nil. A line that reads
// like the header is only one when the text before it parses on its own: the
// same words inside somebody's multi-line string are text.
func findOwnedTable(body string) []int {
	for _, m := range tomlTable.FindAllStringIndex(body, -1) {
		if parsesTOML(body[:m[0]]) {
			return m
		}
	}
	return nil
}

// tableEnd returns the offset where the table starting at start (whose header
// ends at afterHeader) ends. A line opening with `[` is only a header if what
// comes before it is a complete table, so the parser decides: a `[` inside a
// multi-line string is left where it is.
func tableEnd(body string, start, afterHeader int) int {
	for _, m := range tomlNextTable.FindAllStringIndex(body[afterHeader:], -1) {
		end := afterHeader + m[0]
		var parsed map[string]any
		if toml.Unmarshal([]byte(body[start:end]), &parsed) == nil {
			return end
		}
	}
	return len(body)
}

// stripOwnedKeys parses complete assignments to identify their decoded keys.
// Quoted escapes have the same ownership as bare keys; apparent assignments
// inside multi-line strings stay part of the enclosing value.
func stripOwnedKeys(body string) (string, error) {
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for start := 0; start < len(lines); {
		end := start
		var parsed map[string]any
		for {
			if err := toml.Unmarshal([]byte(strings.Join(lines[start:end+1], "\n")), &parsed); err == nil {
				break
			}
			end++
			if end == len(lines) {
				return "", errors.New("incomplete toml assignment")
			}
			parsed = nil
		}
		_, command := parsed["command"]
		_, args := parsed["args"]
		if !command && !args {
			kept = append(kept, lines[start:end+1]...)
		}
		start = end + 1
	}
	return strings.Join(kept, "\n"), nil
}

func parsesTOML(s string) bool {
	var parsed map[string]any
	return toml.Unmarshal([]byte(s), &parsed) == nil
}

func tomlBlock(command string, args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [%s]\n", ServerName, strconv.Quote(command), strings.Join(quoted, ", "))
}

// writeFile replaces the file atomically, keeping the mode it already had.
// A config kept by a dotfile manager is usually a symlink, so the target is
// rewritten rather than replaced by a regular file.
func writeFile(path string, content []byte, mode os.FileMode) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	} else if target, ok := followDanglingLink(path); ok {
		// A link whose target does not exist yet still says where the file
		// belongs; replacing the link with a regular file would break the
		// dotfile manager that made it.
		path = target
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	}
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	// A temp file of its own: two installs running at once must not share one,
	// and the rename onto the config is still atomic.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename has moved it
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// followDanglingLink walks a chain of symlinks to the name at its end, which
// EvalSymlinks refuses to report once that name does not exist. The hop limit
// is the usual guard against a loop.
func followDanglingLink(path string) (string, bool) {
	found := false
	for hop := 0; hop < 32; hop++ {
		next, err := os.Readlink(path)
		if err != nil {
			return path, found
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(path), next)
		}
		path, found = next, true
	}
	return path, found
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ToolNames lists the tools this server exposes, for help text and docs.
func ToolNames() []string {
	names := make([]string, 0, len(listTools()))
	for _, t := range listTools() {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
