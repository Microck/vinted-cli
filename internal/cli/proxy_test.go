package cli_test

import (
	"strings"
	"testing"

	"github.com/Microck/vinted-cli/internal/testutil"
)

func TestAllTrafficGoesThroughTheProxy(t *testing.T) {
	h := newHarness(t)
	proxy, relayed := testutil.ForwardProxy(t)
	hostPort := strings.TrimPrefix(proxy.URL, "http://")
	t.Setenv("VINTED_PROXY", "http://alice:pr0xy-secret@"+hostPort)

	h.login() // includes the token refresh
	h.must("", "search", "nike")
	if got, want := relayed.Load(), int64(h.fake.requestCount()); got == 0 || got != want {
		t.Fatalf("proxy relayed %d of %d requests", got, want)
	}
	if len(h.fake.requestsTo("/web/api/auth/refresh")) == 0 {
		t.Fatal("the refresh did not run, so it was not proven to use the proxy")
	}

	r := h.run("", "doctor")
	if !strings.Contains(r.stdout, "http://"+hostPort) || strings.Contains(r.stdout+r.stderr, "pr0xy-secret") || strings.Contains(r.stdout, "alice") {
		t.Fatalf("doctor must show the proxy without credentials:\n%s\n%s", r.stdout, r.stderr)
	}
}

func TestUnreachableProxyNeverFallsBackToDirect(t *testing.T) {
	for _, scheme := range []string{"http", "socks5h"} {
		h := newHarness(t)
		t.Setenv("VINTED_PROXY", scheme+"://bob:hunter22@"+testutil.ClosedAddr(t))
		r := h.run("", "search", "nike")
		if r.code != 6 || !strings.Contains(r.stderr, "proxy") || strings.Contains(r.stderr, "hunter22") {
			t.Fatalf("%s: exit %d\n%s", scheme, r.code, r.stderr)
		}
		if n := h.fake.requestCount(); n != 0 {
			t.Fatalf("%s: %d requests went direct", scheme, n)
		}
	}
}

func TestBadProxyConfigSendsNothing(t *testing.T) {
	h := newHarness(t)
	t.Setenv("VINTED_PROXY", "ftp://127.0.0.1:21")
	if r := h.run("", "search", "nike"); r.code != 2 || h.fake.requestCount() != 0 {
		t.Fatalf("invalid proxy: exit %d, %d requests\n%s", r.code, h.fake.requestCount(), r.stderr)
	}
	t.Setenv("VINTED_PROXY", "")
	if r := h.run("", "config", "set", "proxy", "socks5h://user:pw@127.0.0.1:1088"); r.code != 2 {
		t.Fatalf("credentials in config.toml accepted: exit %d", r.code)
	}
	if r := h.run("", "config", "set", "proxy", "gopher://x:70"); r.code != 2 {
		t.Fatalf("bad scheme accepted: exit %d", r.code)
	}
	h.must("", "config", "set", "proxy", "socks5h://127.0.0.1:"+strings.Split(testutil.ClosedAddr(t), ":")[1])
	if r := h.run("", "search", "nike"); r.code != 6 || h.fake.requestCount() != 0 {
		t.Fatalf("config proxy unused: exit %d, %d direct requests\n%s", r.code, h.fake.requestCount(), r.stderr)
	}
}
