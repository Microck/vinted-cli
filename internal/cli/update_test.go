package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Microck/vinted-cli/internal/updater"
)

func TestUpdateConsent(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  bool
	}{{"\n", true}, {"y\n", true}, {"YES\n", true}, {"n\n", false}, {"garbage\n", false}, {"", false}} {
		var out bytes.Buffer
		a := App{Version: "0.1.0", Stdin: strings.NewReader(tc.input), Stderr: &out}
		if got := a.updateConsent("v0.2.0"); got != tc.want {
			t.Errorf("%q: %v", tc.input, got)
		}
		if !strings.Contains(out.String(), "[Y/n]") {
			t.Fatal(out.String())
		}
	}
}

func TestAutomaticUpdateSkipsNonInteractiveAndDevelopment(t *testing.T) {
	for _, a := range []*App{{Version: "0.1.0"}, {Version: "dev", Interactive: true}} {
		a.maybeUpdate(context.Background(), a.rootCmd())
	}
}

func TestUpdateDevelopmentBuild(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Execute("dev", []string{"update", "--yes"}, strings.NewReader(""), &out, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "development") {
		t.Fatalf("%d: %s", code, stderr.String())
	}
}

func TestUpdateCheckReportsReleasesAndTheirAbsence(t *testing.T) {
	var answer struct {
		status int
		body   string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	}))
	defer srv.Close()
	defer func(old string) { updater.API = old }(updater.API)
	updater.API = srv.URL

	check := func() (int, string, string) {
		var out, stderr bytes.Buffer
		code := Execute("0.1.0", []string{"update", "--check"}, strings.NewReader(""), &out, &stderr)
		return code, out.String(), stderr.String()
	}

	// No release published yet: GitHub answers 404 on releases/latest.
	answer.status, answer.body = http.StatusNotFound, `{"message":"Not Found"}`
	if code, _, stderr := check(); code != 4 || !strings.Contains(stderr, "no stable release of vinted-cli is published yet") {
		t.Fatalf("no release: exit %d %q", code, stderr)
	}
	answer.status, answer.body = http.StatusOK, `{"tag_name":"v0.2.0"}`
	if code, out, _ := check(); code != 0 || !strings.Contains(out, `"update_available": true`) || !strings.Contains(out, "v0.2.0") {
		t.Fatalf("newer release: exit %d %s", code, out)
	}
	answer.body = `{"tag_name":"v0.1.0"}`
	if code, out, _ := check(); code != 0 || !strings.Contains(out, `"update_available": false`) {
		t.Fatalf("same release: exit %d %s", code, out)
	}
	srv.Close()
	if code, _, stderr := check(); code != 6 || !strings.Contains(stderr, "could not reach GitHub") {
		t.Fatalf("GitHub unreachable: exit %d %q", code, stderr)
	}
}
