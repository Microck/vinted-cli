package galleton

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanEnvPreservesCertificateSettings(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"custom", filepath.Join(t.TempDir(), "custom CA")},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value
			for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
				t.Setenv(key, value)
			}
			blocked := []string{"WALLAPOP_SESSION_TOKEN", "WALLAPOP_GALLETON_TOKEN_FILE", "GITHUB_TOKEN", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"}
			for _, key := range blocked {
				t.Setenv(key, "must-not-inherit")
			}
			env := map[string]string{}
			for _, entry := range cleanEnv() {
				key, value, _ := strings.Cut(entry, "=")
				env[key] = value
			}
			for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
				if got, ok := env[key]; !ok || got != value {
					t.Fatalf("%s: got %q, present=%t, want %q", key, got, ok, value)
				}
			}
			for _, key := range blocked {
				if _, ok := env[key]; ok {
					t.Fatalf("inherited %s", key)
				}
			}
		})
	}
}

func TestCleanEnvOmitsUnsetCertificateSettings(t *testing.T) {
	for _, key := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR"} {
		t.Setenv(key, "restored-after-test")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range cleanEnv() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "SSL_CERT_FILE" || key == "SSL_CERT_DIR" {
			t.Fatalf("invented %s", key)
		}
	}
}
