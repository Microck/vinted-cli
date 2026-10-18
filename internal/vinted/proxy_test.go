package vinted

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Microck/vinted-cli/internal/testutil"
)

func TestParseProxy(t *testing.T) {
	for _, ok := range []string{"socks5h://127.0.0.1:1088", "socks5://user:pw@10.0.0.1:1080", "http://proxy.local:8080", "https://p.example:443/"} {
		if _, err := ParseProxy(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"127.0.0.1:1088", "ftp://x:21", "socks5h://127.0.0.1", "http://:8080", "http://p:8080/path"} {
		if _, err := ParseProxy(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	u, _ := ParseProxy("socks5h://alice:s3cret@127.0.0.1:1088")
	if l := ProxyLabel(u); l != "socks5h://127.0.0.1:1088" {
		t.Fatalf("label %q leaks or mangles", l)
	}
}

func TestProxyTransportRoutesEveryHost(t *testing.T) {
	c := New()
	u, _ := ParseProxy("socks5h://127.0.0.1:1088")
	c.UseProxy(u)
	tr := c.HTTP.Transport.(*http.Transport)
	for _, target := range []string{"https://www.vinted.fr/catalog", "https://api.vinted.fr/x", "https://images1.vinted.net/t/a.webp", "http://127.0.0.1:9/"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		got, err := tr.Proxy(req)
		if err != nil || got == nil || got.String() != "socks5h://127.0.0.1:1088" {
			t.Fatalf("%s goes via %v (%v)", target, got, err)
		}
	}
}

func TestRequestsGoThroughTheProxy(t *testing.T) {
	f, c := newFake(t)
	proxy, relayed := testutil.ForwardProxy(t)
	u, _ := ParseProxy(proxy.URL)
	c.UseProxy(u)
	if _, err := c.Search(context.Background(), SearchParams{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	hits := int64(f.count("GET /") + f.count("GET /catalog"))
	if hits == 0 || relayed.Load() != hits {
		t.Fatalf("proxy relayed %d of %d requests", relayed.Load(), hits)
	}
}

func TestUnreachableProxyFailsClosed(t *testing.T) {
	for _, scheme := range []string{"http", "socks5h"} {
		f, c := newFake(t)
		u, _ := ParseProxy(scheme + "://bob:hunter22@" + testutil.ClosedAddr(t))
		c.UseProxy(u)
		_, err := c.Search(context.Background(), SearchParams{Text: "x"})
		var ve *Error
		if !errors.As(err, &ve) || ve.Kind != KindNetwork || !strings.Contains(ve.Msg, "proxy") {
			t.Fatalf("%s: err = %v", scheme, err)
		}
		if strings.Contains(ve.Msg, "hunter22") {
			t.Fatalf("%s: proxy password in the error: %s", scheme, ve.Msg)
		}
		if n := f.count("GET /") + f.count("GET /catalog"); n != 0 {
			t.Fatalf("%s: %d requests went direct", scheme, n)
		}
	}
}
