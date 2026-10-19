package galleton

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/filelock"
	"github.com/Microck/vinted-cli/internal/galleton/bundle"
)

func TestOpenRejectsRunningEngineVersionMismatch(t *testing.T) {
	for _, duringPoll := range []bool{false, true} {
		name := "existing-endpoint"
		if duringPoll {
			name = "endpoint-changes-during-readiness"
		}
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"VINTED_GALLETON_DIR", "VINTED_GALLETON_URL", "VINTED_GALLETON_TOKEN_FILE"} {
				t.Setenv(key, "")
			}
			dir := filepath.Join(t.TempDir(), "sessions")
			if err := privateDir(dir); err != nil {
				t.Fatal(err)
			}
			host, err := filelock.Acquire(context.Background(), filepath.Join(dir, "host.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer filelock.Release(host)
			stale, err := json.Marshal(endpoint{URL: "http://127.0.0.1:1", Version: "previous-engine"})
			if err != nil {
				t.Fatal(err)
			}
			endpointPath := filepath.Join(dir, "endpoint.json")
			if err := atomicFile(endpointPath, stale, 0600); err != nil {
				t.Fatal(err)
			}
			if duringPoll {
				// Publish the mismatch only after running() has read the matching endpoint.
				// The initial connection returns a health error; polling must detect the
				// subsequently published mismatch rather than waiting out its deadline.
				var once sync.Once
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					once.Do(func() {
						if err := atomicFile(endpointPath, stale, 0600); err != nil {
							t.Error(err)
						}
					})
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				defer server.Close()
				matching, err := json.Marshal(endpoint{URL: server.URL, Version: bundle.Version, Config: testSetup.fingerprint()})
				if err != nil {
					t.Fatal(err)
				}
				if err := atomicFile(endpointPath, matching, 0600); err != nil {
					t.Fatal(err)
				}
				if err := atomicFile(filepath.Join(dir, "api.token"), []byte("test-api-token"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, err := Open(ctx, dir, testSetup)
			if c != nil {
				c.Close()
				t.Fatal("connected to a different engine version")
			}
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected an immediate version error, got %v", err)
			}
			for _, want := range []string{"version", "vinted auth service disable", "vinted auth service enable", "watch/MCP"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing recovery advice %q in %v", want, err)
				}
			}
			leases, err := os.ReadDir(filepath.Join(dir, "leases"))
			if err != nil || len(leases) != 0 {
				t.Fatalf("failed open leaked leases: %v %v", leases, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("attempted to prepare a competing engine")
			}
			raw, err := os.ReadFile(endpointPath)
			if err != nil || string(raw) != string(stale) {
				t.Fatal("modified the running host's endpoint", err)
			}
			other, free, err := filelock.Try(filepath.Join(dir, "host.lock"))
			if other != nil {
				filelock.Release(other)
			}
			if err != nil || free {
				t.Fatal("disturbed the active host lock", err)
			}
		})
	}
}

func TestBundledStaleVersionEndpointRestarts(t *testing.T) {
	dir := bundledRuntime(t)
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	stale, err := json.Marshal(endpoint{URL: "http://127.0.0.1:1", Version: "previous-engine"})
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(filepath.Join(dir, "endpoint.json"), stale, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Open(ctx, dir, testSetup)
	if err != nil {
		t.Fatal("stale endpoint without a live host prevented restart:", err)
	}
	defer c.Close()
	if _, err := running(ctx, dir, ""); err != nil {
		t.Fatal(err)
	}
}
