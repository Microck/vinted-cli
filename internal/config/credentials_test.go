package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCredentialsFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	if err := SaveCredentials(path, Credentials{Profiles: map[string]Session{"one": {GalletonID: "vinted-one", Login: "one"}}}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v, want 0600", st.Mode().Perm())
	}
}

func TestConcurrentLoginsPreserveEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	before := Credentials{Profiles: map[string]Session{"one": {Login: "one"}, "two": {Login: "two"}}}
	if err := SaveCredentials(path, before); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			next := CloneCredentials(before)
			next.Profiles[name] = Session{Login: name, GalletonID: "vinted-" + name}
			if _, err := MergeCredentials(path, before, next); err != nil {
				t.Error(err)
			}
		}(name)
	}
	wg.Wait()
	after, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if after.Profiles[name].GalletonID != "vinted-"+name {
			t.Fatal("concurrent login lost:", name)
		}
	}
}

func TestStaleCommandCannotResurrectLogout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	before := Credentials{Profiles: map[string]Session{"one": {Login: "old"}}}
	if err := SaveCredentials(path, before); err != nil {
		t.Fatal(err)
	}
	if _, err := MergeCredentials(path, before, Credentials{}); err != nil {
		t.Fatal(err)
	}
	next := Credentials{Profiles: map[string]Session{"one": {Login: "new"}}}
	if _, err := MergeCredentials(path, before, next); err == nil {
		t.Fatal("stale command resurrected logout")
	}
}

func TestMergeIgnoresTimeRepresentation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	instant := time.Date(2026, 10, 4, 12, 0, 0, 0, time.FixedZone("", 5*3600+30*60))
	old := Session{Login: "a", GalletonID: "vinted-a", UpdatedAt: instant}
	if err := SaveCredentials(path, Credentials{Profiles: map[string]Session{"one": old}}); err != nil {
		t.Fatal(err)
	}
	before, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	after := CloneCredentials(before)
	s := after.Profiles["one"]
	s.Login = "b"
	after.Profiles["one"] = s
	if _, err := MergeCredentials(path, before, after); err != nil {
		t.Fatal(err)
	}
}

// A file an older version wrote keeps its cookie jar until the next write;
// the profile is marked legacy meanwhile, and the write drops every cookie.
func TestLegacyCookieJarIsFlaggedAndDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	legacy := "[profiles.old]\nuser_id = 1\nlogin = 'old'\n[profiles.old.cookies]\nrefresh_token_web = 'legacy-secret-token'\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.Profiles["old"].Legacy {
		t.Fatal("legacy cookie jar not flagged")
	}
	after := CloneCredentials(before)
	after.Profiles["new"] = Session{GalletonID: "vinted-new", Login: "new"}
	if _, err := MergeCredentials(path, before, after); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "legacy-secret-token") {
		t.Fatal("a credentials write kept a legacy cookie")
	}
}
