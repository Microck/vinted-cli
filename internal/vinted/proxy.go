package vinted

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// ParseProxy validates a proxy URL. http and https proxies tunnel with
// CONNECT; socks5h resolves the Vinted host names on the proxy, so no DNS
// query leaves this machine directly (Go's socks5 does the same).
func ParseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("not a URL like socks5h://127.0.0.1:1080 or http://proxy:8080")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("the scheme must be http, https, socks5 or socks5h")
	}
	if u.Hostname() == "" || u.Port() == "" {
		return nil, errors.New("give the proxy's host and port, e.g. socks5h://127.0.0.1:1080")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("a proxy URL has no path")
	}
	return u, nil
}

// ProxyLabel renders a proxy without its credentials, for doctor and errors.
func ProxyLabel(u *url.URL) string {
	if u == nil {
		return ""
	}
	clean := *u
	clean.User = nil
	clean.Path = ""
	return clean.String()
}

// proxyTransports shares one keep-alive transport per proxy, so the many
// clients of a long-running MCP server reuse their connections.
var proxyTransports sync.Map // proxy URL string -> *http.Transport

// UseProxy sends every request of the client through u, and only through
// u: the transport has no fallback, so an unreachable proxy fails the
// request (KindNetwork) instead of going direct. NO_PROXY and the
// HTTP(S)_PROXY variables no longer apply.
func (c *Client) UseProxy(u *url.URL) {
	key := u.String()
	tr, ok := proxyTransports.Load(key)
	if !ok {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = http.ProxyURL(u)
		tr, _ = proxyTransports.LoadOrStore(key, t)
	}
	c.HTTP.Transport = tr.(*http.Transport)
	if u.User != nil {
		c.Redact(key)
		if pw, set := u.User.Password(); set {
			c.Redact(pw)
		}
	}
	c.mu.Lock()
	c.proxy = ProxyLabel(u)
	c.mu.Unlock()
}
