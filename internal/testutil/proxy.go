// Package testutil holds helpers shared by tests of several packages. Nothing
// here talks to anything but the local machine.
package testutil

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// ForwardProxy is a plain HTTP forward proxy that counts the requests it
// relays. It strips Proxy-Authorization and reaches the target directly.
func ForwardProxy(t testing.TB) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var relayed atomic.Int64
	direct := &http.Transport{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "not a proxy request", http.StatusBadRequest)
			return
		}
		relayed.Add(1)
		out, _ := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		out.Header = r.Header.Clone()
		out.Header.Del("Proxy-Authorization")
		resp, err := direct.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(direct.CloseIdleConnections)
	return srv, &relayed
}

// ClosedAddr is a local host:port nothing listens on.
func ClosedAddr(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
