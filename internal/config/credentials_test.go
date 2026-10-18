package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCredentialsFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	if err := SaveCredentials(path, Credentials{Profiles: map[string]Session{"one": {Cookies: map[string]string{"access_token_web": "a"}}}}); err != nil {
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
			next.Profiles[name] = Session{Login: name, Cookies: map[string]string{"access_token_web": name}}
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
		if after.Profiles[name].Cookies["access_token_web"] != name {
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
	old := Session{Login: "a", SessionExpires: instant.Add(time.Hour), UpdatedAt: instant}
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

func TestRotateSessionMergesCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.toml")
	if err := SaveCredentials(path, Credentials{Profiles: map[string]Session{"one": {Cookies: map[string]string{"access_token_web": "old", "datadome": "d"}}}}); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := RotateSession(path, "one", map[string]string{"access_token_web": "new"}, exp); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateSession(path, "gone", map[string]string{"access_token_web": "x"}, exp); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateSession(path, "one", map[string]string{"datadome": "d2"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	s := creds.Profiles["one"]
	if s.Cookies["access_token_web"] != "new" || s.Cookies["datadome"] != "d2" || !s.SessionExpires.Equal(exp) {
		t.Fatalf("rotation not stored: %+v", s.SessionExpires)
	}
	if _, ok := creds.Profiles["gone"]; ok {
		t.Fatal("rotation resurrected a logged-out profile")
	}
}
