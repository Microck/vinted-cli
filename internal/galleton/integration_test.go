package galleton

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/galleton/bundle"
)

// Exercise the pinned executable and official SDK, not a mock daemon. The TLS
// provider behaves like Vinted's web session: renewal rotates both token
// cookies and accepts only the latest refresh token, and API calls need the
// latest access token. Lost rotation becomes visible after a restart. No
// real Vinted endpoint is contacted.
func TestRealDaemonPersistsRotationAcrossRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("hermetic SSL_CERT_FILE trust is Linux-only; native lifecycle tests run on every supported OS")
	}
	if len(bundle.Binary) == 0 {
		t.Skip("requires generated bundle")
	}
	var mu sync.Mutex
	refresh, access := "initial-refresh-credential", "initial-access-credential"
	calls := 0
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/web/api/auth/refresh" {
			ck, err := r.Cookie("refresh_token_web")
			if r.Method != http.MethodPost || err != nil || ck.Value != refresh {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			calls++
			refresh = fmt.Sprintf("rotated-refresh-credential-%d", calls)
			access = fmt.Sprintf("rotated-access-credential-%d", calls)
			http.SetCookie(w, &http.Cookie{Name: "refresh_token_web", Value: refresh, Path: "/", Secure: true, HttpOnly: true, MaxAge: 7 * 86400})
			http.SetCookie(w, &http.Cookie{Name: "access_token_web", Value: access, Path: "/", Secure: true, MaxAge: 3600})
			_ = json.NewEncoder(w).Encode(map[string]any{"expires_in": 3600})
			return
		}
		ck, err := r.Cookie("access_token_web")
		if err != nil || ck.Value != access {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// A guest token on an ordinary page must not replace the login.
		http.SetCookie(w, &http.Cookie{Name: "access_token_web", Value: "guest-access", Path: "/"})
		fmt.Fprintf(w, `{"user":{"id":1,"seen":%q}}`, ck.Value)
	}))
	defer provider.Close()
	dir := filepath.Join(t.TempDir(), "vault")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	binary, err := engineBinary(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(t.TempDir(), "provider.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: provider.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", cert)
	t.Setenv("SSL_CERT_DIR", t.TempDir())
	cfg := []byte(fmt.Sprintf(`{"providers":[{"name":"vinted","kind":"http","origins":[%q],"refresh_url":%q,"refresh_method":"POST","body_format":"json","refresh_interval_seconds":3600,"response":{"expires_in_pointer":"/expires_in","require_set_cookie":true},"protected_cookies":["access_token_web","refresh_token_web"]}]}`, provider.URL, provider.URL+"/web/api/auth/refresh"))
	configPath := filepath.Join(dir, "test-adapters.json")
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	start := func() (*Client, func()) {
		t.Helper()
		init := exec.Command(binary, "init", "--dir", dir)
		init.Env = cleanEnv()
		noWindow(init)
		if err := init.Run(); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "serve", "--dir", dir, "--config", configPath, "--listen", "127.0.0.1:0")
		cmd.Env = cleanEnv()
		noWindow(cmd)
		pipe, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		address := make(chan string, 1)
		go func() {
			sc := bufio.NewScanner(pipe)
			for sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) > 3 && strings.HasPrefix(sc.Text(), "Galleton listening on ") {
					address <- "http://" + fields[3]
					break
				}
			}
		}()
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		var base string
		select {
		case base = <-address:
		case <-time.After(10 * time.Second):
			t.Fatal("daemon did not start")
		}
		token, err := os.ReadFile(filepath.Join(dir, "api.token"))
		if err != nil {
			t.Fatal(err)
		}
		c, err := New(base, string(token))
		if err != nil {
			t.Fatal(err)
		}
		return c, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("daemon did not drain")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, stop := start()
	in := Credentials{Provider: "vinted", CookieOrigin: provider.URL, CookieHeader: "access_token_web=initial-access-credential; refresh_token_web=initial-refresh-credential"}
	if _, err = c.Connect(ctx, "account", in); err != nil {
		t.Fatal(err)
	}
	me := func(want string) {
		t.Helper()
		resp, err := c.Request(ctx, "account", "GET", provider.URL+"/api/v2/users/current", map[string]string{"Accept": "application/json"}, nil)
		if err != nil || resp.Status != 200 || !strings.Contains(string(resp.Body), want) {
			t.Fatalf("managed request: %d %s %v", resp.Status, resp.Body, err)
		}
	}
	// Import schedules an immediate renewal, so the first call already
	// carries the rotated access token.
	me("rotated-access-credential-1")
	me("rotated-access-credential-1")
	mu.Lock()
	count := calls
	mu.Unlock()
	if count != 1 {
		t.Fatalf("valid cookies caused %d renewals", count)
	}
	stop()
	files, err := filepath.Glob(filepath.Join(dir, "*.session-v2"))
	if err != nil || len(files) != 1 {
		t.Fatalf("vault files: %v %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil || strings.Contains(string(raw), "-credential") {
		t.Fatal("provider secrets were not encrypted", err)
	}
	c, stop = start()
	defer stop()
	if _, err := c.Refresh(ctx, "account"); err != nil {
		t.Fatal("lost rotated cookie after restart:", err)
	}
	me("rotated-access-credential-2")
	if err := c.Forget(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx, "account"); !IsStatus(err, 404) {
		t.Fatal("session not forgotten", err)
	}
}
