package vinted

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Vinted's catalog and item pages have no JSON API on the public web host;
// the data only exists in the React Server Components (RSC) "flight" payload
// that Next.js streams into the page as
//
//	<script>self.__next_f.push([1,"<JS string literal>"])</script>
//
// chunks. Concatenated, the chunks form rows of `<hex id>:<payload>\n`, where
// a payload is JSON (optionally prefixed by a one-letter row tag such as I for
// module imports) or `T<hex byte length>,<raw text>`. Values inside the JSON
// reference other rows as "$<hex id>[:path:to:field]" and mark absent values
// as "$undefined". rscPage decodes all of that into plain JSON values.

var nextFPushRe = regexp.MustCompile(`self\.__next_f\.push\(\[1,("(?:[^"\\]|\\.)*")\]\)`)

type rscPage struct {
	rows map[string]json.RawMessage
	// cache holds rows already decoded into generic values.
	cache map[string]any
}

// parseRSC extracts the flight rows from a server-rendered page. It returns
// nil when the page carries no flight payload (a bot wall or an error page).
func parseRSC(page []byte) *rscPage {
	stream := rscStream(page)
	if len(stream) == 0 {
		return nil
	}
	return &rscPage{rows: splitRSCRows(stream), cache: map[string]any{}}
}

// rscStream concatenates the page's flight chunks; values may straddle chunks.
func rscStream(page []byte) []byte {
	var stream bytes.Buffer
	for _, m := range nextFPushRe.FindAllSubmatch(page, -1) {
		var chunk string
		if err := json.Unmarshal(m[1], &chunk); err != nil {
			// The literal is a JS string; the few JS-only escapes (\x..) are
			// not used by Next.js, so a JSON decode failure means a chunk we
			// cannot read. Skip it rather than poison the rest of the stream.
			continue
		}
		stream.WriteString(chunk)
	}
	return stream.Bytes()
}

// splitRSCRows splits a flight stream into its JSON rows, keyed by hex id.
// Text rows and non-JSON rows are skipped; nothing on the pages Vinted
// renders needs them.
func splitRSCRows(b []byte) map[string]json.RawMessage {
	rows := map[string]json.RawMessage{}
	for len(b) > 0 {
		colon := bytes.IndexByte(b, ':')
		// Hint rows (":HL[...]") have an empty id; skip past them like any row.
		if colon < 0 {
			break
		}
		id := string(b[:colon])
		b = b[colon+1:]
		if len(b) > 0 && b[0] == 'T' {
			comma := bytes.IndexByte(b, ',')
			if comma < 0 {
				break
			}
			n, err := strconv.ParseInt(string(b[1:comma]), 16, 64)
			if err != nil || int(n) > len(b)-comma-1 {
				break
			}
			b = b[comma+1+int(n):]
			continue
		}
		nl := bytes.IndexByte(b, '\n')
		line := b
		if nl >= 0 {
			line, b = b[:nl], b[nl+1:]
		} else {
			b = nil
		}
		// Strip a row tag (I, HL, E, ...) but not the JSON literals.
		if len(line) > 0 && isASCIILetter(line[0]) && !bytes.HasPrefix(line, []byte("null")) && !bytes.HasPrefix(line, []byte("true")) && !bytes.HasPrefix(line, []byte("false")) {
			i := 0
			for i < len(line) && isASCIILetter(line[i]) {
				i++
			}
			line = line[i:]
		}
		if json.Valid(line) {
			rows[id] = json.RawMessage(line)
		}
	}
	return rows
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// row decodes one row into a generic value, unresolved.
func (p *rscPage) row(id string) (any, bool) {
	if v, ok := p.cache[id]; ok {
		return v, true
	}
	raw, ok := p.rows[id]
	if !ok {
		return nil, false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil, false
	}
	p.cache[id] = v
	return v, true
}

var rscRefRe = regexp.MustCompile(`^\$@?([0-9a-f]+)((?::[^:]+)*)$`)

// resolve replaces "$undefined" with nil and row references with the value
// they point at, recursively. depth bounds reference chains.
func (p *rscPage) resolve(v any, depth int) any {
	if depth > 24 {
		return nil
	}
	switch t := v.(type) {
	case string:
		if !strings.HasPrefix(t, "$") {
			return t
		}
		if t == "$undefined" {
			return nil
		}
		if strings.HasPrefix(t, "$$") {
			return t[1:]
		}
		m := rscRefRe.FindStringSubmatch(t)
		if m == nil {
			return t
		}
		target, ok := p.row(m[1])
		if !ok {
			return nil
		}
		for _, key := range strings.Split(strings.TrimPrefix(m[2], ":"), ":") {
			if key == "" {
				continue
			}
			target = p.resolve(target, depth+1)
			target = childOf(target, key)
		}
		return p.resolve(target, depth+1)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = p.resolve(x, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = p.resolve(x, depth+1)
		}
		return out
	}
	return v
}

func childOf(v any, key string) any {
	switch t := v.(type) {
	case map[string]any:
		return t[key]
	case []any:
		// A React element is serialized as ["$", type, key, props]; paths
		// name its parts.
		if len(t) >= 4 && t[0] == "$" {
			switch key {
			case "type":
				return t[1]
			case "key":
				return t[2]
			case "props":
				return t[3]
			}
		}
		if i, err := strconv.Atoi(key); err == nil && i >= 0 && i < len(t) {
			return t[i]
		}
	}
	return nil
}

// find walks every row, in id order, and returns the first object match
// accepts, unresolved (resolve it before decoding).
func (p *rscPage) find(match func(map[string]any) bool) map[string]any {
	ids := make([]string, 0, len(p.rows))
	for id := range p.rows {
		ids = append(ids, id)
	}
	sortHexIDs(ids)
	for _, id := range ids {
		v, ok := p.row(id)
		if !ok {
			continue
		}
		if found := walkFind(v, match); found != nil {
			return found
		}
	}
	return nil
}

func walkFind(v any, match func(map[string]any) bool) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if match(t) {
			return t
		}
		// Sorted keys keep the first match stable across runs.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if found := walkFind(t[k], match); found != nil {
				return found
			}
		}
	case []any:
		for _, x := range t {
			if found := walkFind(x, match); found != nil {
				return found
			}
		}
	}
	return nil
}

func sortHexIDs(ids []string) {
	key := func(s string) int64 {
		n, err := strconv.ParseInt(s, 16, 64)
		if err != nil {
			return 1 << 62
		}
		return n
	}
	sort.Slice(ids, func(i, j int) bool { return key(ids[i]) < key(ids[j]) })
}

// decode resolves v and converts it into out through JSON.
func (p *rscPage) decode(v any, out any) error {
	b, err := json.Marshal(p.resolve(v, 0))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func hasKeys(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	return true
}
