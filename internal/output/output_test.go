package output

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Microck/vinted-cli/internal/vinted"
)

type row struct {
	Zeta  string `json:"zeta"`
	Alpha string `json:"alpha"`
}

func render(t *testing.T, f Format, v any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := (Printer{W: &buf, Format: f}).Print(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestJSONIsDataNotHTML(t *testing.T) {
	v := []row{{Zeta: "Capes & ponchos", Alpha: "<b>"}}
	for _, f := range []Format{JSON, JSONL, Pretty} {
		if out := render(t, f, v); strings.Contains(out, `\u0026`) || strings.Contains(out, `\u003c`) || !strings.Contains(out, "Capes & ponchos") {
			t.Errorf("%s escaped html:\n%s", f, out)
		}
	}
}

func TestJSONLKeepsFieldOrderAndSplitsArrays(t *testing.T) {
	out := render(t, JSONL, []row{{"z1", "a1"}, {"z2", "a2"}})
	if out != "{\"zeta\":\"z1\",\"alpha\":\"a1\"}\n{\"zeta\":\"z2\",\"alpha\":\"a2\"}\n" {
		t.Fatalf("jsonl:\n%s", out)
	}
	if out := render(t, JSONL, row{"z", "a"}); out != "{\"zeta\":\"z\",\"alpha\":\"a\"}\n" {
		t.Fatalf("jsonl of one value:\n%s", out)
	}
	if out := render(t, JSONL, []row{}); out != "" {
		t.Fatalf("jsonl of an empty list prints nothing, got %q", out)
	}
}

func TestToonAndPrettyFallback(t *testing.T) {
	if out := render(t, Toon, row{"z", "a"}); !strings.Contains(out, "zeta: z") || !strings.Contains(out, "alpha: a") {
		t.Fatalf("toon:\n%s", out)
	}
	// A type without a Pretty method falls back to indented JSON.
	if out := render(t, Pretty, row{"z", "a"}); out != "{\n  \"zeta\": \"z\",\n  \"alpha\": \"a\"\n}\n" {
		t.Fatalf("pretty fallback:\n%s", out)
	}
}

func TestErrorEnvelopeAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		cat  string
	}{
		{errors.New("boom"), 1, "generic"},
		{Usagef("bad flag"), 2, "usage"},
		{&vinted.Error{Kind: vinted.KindAuth, Msg: "no session"}, 3, "auth"},
		{vinted.NotFound("no item %d", 1), 4, "not_found"},
		{&vinted.Error{Kind: vinted.KindBlocked, Msg: "blocked"}, 5, "blocked"},
		{&vinted.Error{Kind: vinted.KindNetwork, Msg: "down"}, 6, "network"},
		{&vinted.Error{Kind: vinted.KindAPIChanged, Msg: "shape", Endpoint: "GET /x?a=1&b=2"}, 7, "api_changed"},
	} {
		var buf bytes.Buffer
		if code := PrintError(&buf, tc.err, ErrJSON, "test", "vinted x"); code != tc.code {
			t.Errorf("%v: exit %d, want %d", tc.err, code, tc.code)
		}
		if !strings.Contains(buf.String(), `"category":"`+tc.cat+`"`) || strings.Contains(buf.String(), `\u0026`) {
			t.Errorf("%v: envelope %s", tc.err, buf.String())
		}
	}
	var buf bytes.Buffer
	PrintError(&buf, &vinted.Error{Kind: vinted.KindAPIChanged, Msg: "shape"}, ErrText, "test", "vinted x")
	if !strings.Contains(buf.String(), "issues/new?") || !strings.Contains(buf.String(), "--debug") {
		t.Fatalf("api change text:\n%s", buf.String())
	}
}
