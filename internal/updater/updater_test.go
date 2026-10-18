package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, test := range []struct {
		latest, current string
		want            bool
	}{
		{"v0.2.0", "0.1.0", true}, {"v1.10.0", "1.9.0", true},
		{"v0.1.0", "0.1.0", false}, {"v0.1.0", "0.2.0", false},
		{"v0.2.0", "dev", false}, {"v0.2.0-rc.1", "0.1.0", false},
		{"v0.2.0", "0.2.0-rc.1", false}, {"v01.2.3", "1.2.2", false},
	} {
		if got := Newer(test.latest, test.current); got != test.want {
			t.Errorf("Newer(%q,%q)=%v", test.latest, test.current, got)
		}
	}
}

func TestVerify(t *testing.T) {
	data := []byte("binary")
	sums := []byte(fmt.Sprintf("%x  binary.tar.gz\n", sha256.Sum256(data)))
	if err := Verify(data, sums, "binary.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if Verify([]byte("tampered"), sums, "binary.tar.gz") == nil {
		t.Fatal("accepted tampered archive")
	}
	if Verify(data, sums, "other.tar.gz") == nil {
		t.Fatal("accepted absent checksum")
	}
}

func TestExtract(t *testing.T) {
	var archive bytes.Buffer
	g := gzip.NewWriter(&archive)
	tw := tar.NewWriter(g)
	for _, name := range []string{"../../vinted", "vinted"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: 3}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte("bin"))
	}
	_ = tw.Close()
	_ = g.Close()
	got, err := Extract(archive.Bytes(), ".tar.gz")
	if err != nil || string(got) != "bin" {
		t.Fatalf("%q %v", got, err)
	}
	var za bytes.Buffer
	zw := zip.NewWriter(&za)
	w, _ := zw.Create("vinted.exe")
	_, _ = w.Write([]byte("exe"))
	_ = zw.Close()
	got, err = Extract(za.Bytes(), ".zip")
	if err != nil || string(got) != "exe" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err = Extract([]byte("invalid"), ".zip"); err == nil {
		t.Fatal("accepted bad archive")
	}
}

func TestReplace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vinted")
	if err := os.WriteFile(p, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Replace(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "new" {
		t.Fatal(string(b))
	}
}

func TestManager(t *testing.T) {
	for _, tc := range []struct{ path, command string }{
		{"/usr/local/lib/node_modules/vinted-cli/vinted", "npm"},
		{"/opt/homebrew/Caskroom/vinted/1.0.0/vinted", "brew"},
		{"C:/Users/test/scoop/apps/vinted/current/vinted.exe", "scoop"},
		{"/usr/bin/vinted", "yay"}, {"/home/test/.local/bin/vinted", ""},
	} {
		c := Manager(tc.path)
		got := ""
		if len(c) > 0 {
			got = c[0]
		}
		if got != tc.command {
			t.Errorf("%s: %s", tc.path, got)
		}
	}
}

func TestGetRejectsErrorsAndOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{{500, "error"}, {200, "oversized"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := get(context.Background(), server.Client(), server.URL, 3)
		server.Close()
		if err == nil {
			t.Fatal("accepted invalid response")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallVerifiesBeforeReplacing(t *testing.T) {
	var archive bytes.Buffer
	g := gzip.NewWriter(&archive)
	tw := tar.NewWriter(g)
	_ = tw.WriteHeader(&tar.Header{Name: "vinted", Mode: 0755, Size: 3})
	_, _ = tw.Write([]byte("new"))
	_ = tw.Close()
	_ = g.Close()
	// Run on the host platform; Windows archive is covered separately above.
	if runtime.GOOS == "windows" {
		t.Skip("tar integration uses Unix archive")
	}
	name := fmt.Sprintf("vinted_0.2.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	for _, tampered := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "vinted")
		_ = os.WriteFile(path, []byte("old"), 0755)
		sums := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive.Bytes()), name)
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var data []byte
			switch req.URL.Path {
			case "/repos/Microck/vinted-cli/releases/latest":
				data = []byte(`{"tag_name":"v0.2.0"}`)
			case "/Microck/vinted-cli/releases/download/v0.2.0/checksums.txt":
				data = []byte(sums)
			case "/Microck/vinted-cli/releases/download/v0.2.0/" + name:
				data = archive.Bytes()
				if tampered {
					data = []byte("tampered")
				}
			default:
				t.Fatalf("unexpected URL: %s", req.URL)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
		})}
		r, err := Latest(context.Background(), client)
		if err != nil {
			t.Fatal(err)
		}
		err = Install(context.Background(), client, r, path)
		got, _ := os.ReadFile(path)
		if tampered {
			if err == nil || string(got) != "old" {
				t.Fatalf("tampered update changed binary: %s %v", got, err)
			}
		} else if err != nil || string(got) != "new" {
			t.Fatalf("update: %s %v", got, err)
		}
	}
}
