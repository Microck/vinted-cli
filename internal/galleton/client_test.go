package galleton

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microck/galleton/cookiefile"
)

func TestValidateConnection(t *testing.T) {
	for _, base := range []string{
		"https://127.0.0.1:8766", "http://localhost:8766", "http://example.com",
		"http://127.0.0.1/x", "http://127.0.0.1?", "http://127.0.0.1?x=y",
		"http://127.0.0.1/#fragment", "http://user:pass@127.0.0.1", "http://192.168.1.1",
	} {
		t.Run(base, func(t *testing.T) {
			if _, err := New(base, "secret"); err == nil {
				t.Fatal("accepted unsafe daemon URL")
			}
		})
	}
	for _, token := range []string{"", "  ", "one\ntwo", "one\rtwo"} {
		if _, err := New(DefaultURL, token); err == nil {
			t.Fatal("accepted invalid API token")
		}
	}
	for _, base := range []string{"", "http://127.0.0.1:8766/", "http://[::1]:8766"} {
		if _, err := New(base, "secret\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionIDValidation(t *testing.T) {
	c, err := New("http://127.0.0.1:1", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../another", "a/b", "a?b", "a%2fb", "你好", strings.Repeat("a", 65)} {
		if _, err := c.Status(context.Background(), id); err == nil {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
}

func TestAPIContract(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer local-secret" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing local API authentication or content type")
		}
		switch {
		case r.Method == http.MethodPut:
			var in Credentials
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in.Provider != "vinted" || len(in.Cookies) != 1 || in.Cookies[0].Name != "refresh_token_web" || !in.Replace || in.ExpectedRevision == nil || *in.ExpectedRevision != 7 {
				t.Errorf("unexpected import: %+v", in)
			}
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/request"):
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["url"] != "https://www.vinted.com/api/v2/users/current" || in["method"] != "GET" {
				t.Errorf("wrong managed request %v", in)
			}
			fmt.Fprint(w, `{"status":200,"headers":{"Content-Type":["application/json"]},"body_base64":"eyJvayI6dHJ1ZX0=","revision":8}`)
			return
		case strings.HasSuffix(r.URL.Path, "/pause"):
			if r.Method == http.MethodPost {
				var in map[string]int
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["seconds"] != 90 {
					t.Errorf("pause seconds %v", in)
				}
			}
			fmt.Fprint(w, `{"provider":"vinted"}`)
			return
		}
		fmt.Fprint(w, `{"id":"test","provider":"vinted","revision":8,"status":"ready"}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "local-secret")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rev := uint64(7)
	in := Credentials{Provider: "vinted", Replace: true, ExpectedRevision: &rev}
	in.Cookies = append(in.Cookies, cookiefile.Cookie{Name: "refresh_token_web", Value: "secret", Domain: "www.vinted.com"})
	meta, err := c.Connect(ctx, "test", in)
	if err != nil || meta.Revision != 8 {
		t.Fatalf("connect: %+v, %v", meta, err)
	}
	if _, err := c.Status(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Request(ctx, "test", "GET", "https://www.vinted.com/api/v2/users/current", map[string]string{"Accept": "application/json"}, nil)
	if err != nil || resp.Status != 200 || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("request: %+v, %v", resp, err)
	}
	if err := c.Pause(ctx, "vinted", 89500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(ctx, "vinted"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Refresh(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	want := "PUT /v1/sessions/test,GET /v1/sessions/test,POST /v1/sessions/test/request,POST /v1/providers/vinted/pause,DELETE /v1/providers/vinted/pause,POST /v1/sessions/test/refresh,DELETE /v1/sessions/test"
	if strings.Join(calls, ",") != want {
		t.Fatalf("calls = %v", calls)
	}
}

func TestPausedErrorKeepsRetryAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":"provider_paused","message":"paused","retry_at":"2030-01-02T03:04:05Z"}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "secret")
	_, err := c.Request(context.Background(), "test", "GET", "https://www.vinted.com/", nil, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != "provider_paused" || !apiErr.RetryAt.Equal(time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("paused error: %#v", err)
	}
}

func TestRedirectDoesNotLeakToken(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer destination.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "local-secret")
	_, err := c.Status(context.Background(), "test")
	if err == nil || called {
		t.Fatalf("redirect followed or not reported: %v, called=%v", err, called)
	}

}

func TestErrorsAreBoundedAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"daemon", 409, `{"error":{"code":"renewal_uncertain","message":"cookie-secret"}}`},
		{"malformed", 200, `not-json cookie-secret`},
		{"oversized", 200, strings.Repeat("x", (8<<20)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			c, _ := New(srv.URL, "local-secret")
			_, err := c.Status(context.Background(), "test")
			if err == nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 200 {
				t.Fatalf("unsafe error: %v", err)
			}
			if tc.status == 409 {
				var apiErr *Error
				if !errors.As(err, &apiErr) || apiErr.Code != "renewal_uncertain" {
					t.Fatalf("lost error classification: %v", err)
				}
			}
		})
	}
}

func TestCancellationAndMissingSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"not_found"}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Status(ctx, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := c.Forget(context.Background(), "missing"); err != nil {
		t.Fatalf("forget should be idempotent: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "api.token")
	if err := os.WriteFile(file, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VINTED_GALLETON_DIR", dir)
	t.Setenv("VINTED_GALLETON_URL", "")
	t.Setenv("VINTED_GALLETON_TOKEN_FILE", "")
	c, err := FromEnv()
	if err != nil || c == nil || !Configured() {
		t.Fatalf("from dir: %v", err)
	}
	t.Setenv("VINTED_GALLETON_DIR", filepath.Join(dir, "missing"))
	t.Setenv("VINTED_GALLETON_TOKEN_FILE", file)
	if _, err := FromEnv(); err != nil {
		t.Fatal("explicit token file must take precedence:", err)
	}
	t.Setenv("VINTED_GALLETON_TOKEN_FILE", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("missing token file accepted")
	}
}
