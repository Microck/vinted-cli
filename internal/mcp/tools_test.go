package mcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A harness may stop the server with a signal while stdin is still open, which
// is what the cancelled context stands for here. The pipe is never written to,
// so the read is blocked when the cancel lands.
func TestServeReturnsWhenTheContextIsCancelledOnIdleStdin(t *testing.T) {
	in, hold := io.Pipe()
	defer hold.Close()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Version: "test"}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, in, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		// The cancellation is reported, not swallowed: the CLI turns it into
		// exit 130.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve stayed blocked on stdin after the context was cancelled")
	}
}

// The header this CLI owns can also appear as somebody's text. Rewriting there
// would bury the entry inside a string and leave codex without a server.
func TestFindOwnedTableSkipsAHeaderInsideAString(t *testing.T) {
	body := "note = \"\"\"\n[mcp_servers.vinted]\n\"\"\"\n\n[mcp_servers.vinted]\ncommand = \"real\"\n"
	loc := findOwnedTable(body)
	if loc == nil {
		t.Fatal("the real header was not found")
	}
	if rest := body[loc[0]:]; !strings.HasPrefix(rest, "[mcp_servers.vinted]\ncommand = \"real\"") {
		t.Fatalf("matched the wrong occurrence: %q", rest)
	}
	if findOwnedTable("note = \"\"\"\n[mcp_servers.vinted]\n\"\"\"\n") != nil {
		t.Error("a header that exists only inside a string was taken as real")
	}
}

// tableEnd has to tell a header from a `[` that only looks like one, which is
// why it asks the parser rather than the regex alone.
func TestTableEndStopsAtRealHeaders(t *testing.T) {
	body := "[mcp_servers.vinted]\nargs = [\"\"\"\n[profile]\n\"\"\"]\nenabled = true\n\n[mcp_servers.other]\ncommand = \"other\"\n"
	loc := tomlTable.FindStringIndex(body)
	if loc == nil {
		t.Fatal("the owned header did not match")
	}
	end := tableEnd(body, loc[0], loc[1])
	if rest := body[end:]; rest != "[mcp_servers.other]\ncommand = \"other\"\n" {
		t.Fatalf("the table ended at the wrong place, rest was %q", rest)
	}
}

// The server itself is tested through the stdio transport in internal/cli.
// What is left here is the argv mapping: positionals go last behind --, and
// only the flags the input carries are emitted.
func TestToolsMapToTheirCommands(t *testing.T) {
	cases := []struct {
		tool Tool
		in   map[string]any
		want string
	}{
		{searchTool, map[string]any{"text": "levis 501", "price_to": float64(30), "brand": []any{"53", "88"}, "condition": []any{"good"}}, "search --price-to 30 --brand 53 --brand 88 --condition good -- levis 501"},
		{searchTool, map[string]any{"filter": []any{"material_ids[]=1", "material_ids[]=2,3"}, "currency": "USD", "price_to": float64(20)}, "search --price-to 20 --currency USD --filter material_ids[]=1 --filter material_ids[]=2,3"},
		{searchTool, map[string]any{"sim_lock": []any{"1"}, "video_game_rating": []any{"2"}, "attr": []any{"fit=4,5"}, "pages": float64(2)}, "search --video-game-rating 2 --sim-lock 1 --attr fit=4,5 --pages 2"},
		{searchFiltersTool, map[string]any{"catalog": []any{"1845"}, "code": []any{"size", "status"}, "text": "jeans"}, "search filters --catalog 1845 --code size --code status -- jeans"},
		{userItemsTool, map[string]any{"user": "123456789", "page": float64(2)}, "user items --page 2 -- 123456789"},
		{chatListTool, map[string]any{"unread": true, "limit": float64(5)}, "chat list --unread --limit 5"},
		{chatShowTool, map[string]any{"conversation": "123"}, "chat show -- 123"},
		{categoryListTool, nil, "category list"},
	}
	for _, tc := range cases {
		argv, err := tc.tool.argv(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool.Name, err)
		}
		if got := strings.Join(argv, " "); got != tc.want {
			t.Errorf("%s: argv %q, want %q", tc.tool.Name, got, tc.want)
		}
	}
	if _, err := itemShowTool.argv(map[string]any{}); err == nil {
		t.Error("item_show without item should be refused")
	}
	if _, err := searchTool.argv(map[string]any{"page": 1.5}); err == nil {
		t.Error("a fractional page should be refused")
	}
}

func TestWithheldToolsExplainThemselves(t *testing.T) {
	if _, ok := lookupTool("chat_send"); ok {
		t.Fatal("chat_send must not be a tool")
	}
	if msg := unknownToolMessage("chat_send"); !strings.Contains(msg, "not offered") {
		t.Fatalf("chat_send refusal %q does not say why", msg)
	}
}
