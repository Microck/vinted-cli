package config

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/Microck/vinted-cli/internal/filelock"
)

func CloneCredentials(in Credentials) Credentials {
	out := Credentials{Profiles: map[string]Session{}}
	for k, v := range in.Profiles {
		v.Cookies = maps.Clone(v.Cookies)
		out.Profiles[k] = v
	}
	return out
}

// sameSession compares persisted timestamps by instant, not by time-zone
// pointers or monotonic clock data that may differ after a TOML round trip.
func sameSession(a, b Session) bool {
	return maps.Equal(a.Cookies, b.Cookies) && a.UserID == b.UserID && a.Login == b.Login &&
		a.SessionExpires.Equal(b.SessionExpires) && a.UpdatedAt.Equal(b.UpdatedAt)
}

// LockCredentials serialises every read-modify-write of credentials.toml across
// processes: a watch timer refreshing tokens and a manual command must not
// overwrite each other's rotated cookies, nor refresh with the same token. The
// lock is not re-entrant. Waiting gives up after 30s or when ctx ends.
func LockCredentials(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock, err := filelock.Acquire(ctx, path+".lock")
	if err != nil {
		return nil, err
	}
	return func() { filelock.Release(lock) }, nil
}

// MergeCredentials preserves unrelated profiles saved by concurrent commands.
// A stale command cannot resurrect a deleted session or overwrite a newer login.
func MergeCredentials(path string, before, after Credentials) (Credentials, error) {
	unlock, err := LockCredentials(context.Background(), path)
	if err != nil {
		return Credentials{}, err
	}
	defer unlock()
	disk, err := LoadCredentials(path)
	if err != nil {
		return Credentials{}, err
	}
	keys := map[string]bool{}
	for k := range before.Profiles {
		keys[k] = true
	}
	for k := range after.Profiles {
		keys[k] = true
	}
	for k := range keys {
		old, had := before.Profiles[k]
		next, has := after.Profiles[k]
		if had == has && sameSession(old, next) {
			continue
		}
		current, exists := disk.Profiles[k]
		if exists != had || !sameSession(current, old) {
			if !has && !exists {
				continue
			}
			return Credentials{}, errors.New("profile session changed concurrently; retry the command")
		}
		if has {
			disk.Profiles[k] = next
		} else {
			delete(disk.Profiles, k)
		}
	}
	if err := SaveCredentials(path, disk); err != nil {
		return Credentials{}, err
	}
	return disk, nil
}

// RotateSession stores rotated cookies for one profile under the lock. The
// cookies are merged over what is on disk, so cookies another process added
// or rotated meanwhile survive. A zero expires keeps the stored access token
// expiry. A profile logged out meanwhile stays logged out.
func RotateSession(path, profile string, cookies map[string]string, expires time.Time) (Credentials, error) {
	unlock, err := LockCredentials(context.Background(), path)
	if err != nil {
		return Credentials{}, err
	}
	defer unlock()
	return RotateSessionLocked(path, profile, cookies, expires)
}

// RotateSessionLocked is RotateSession for a caller already holding
// LockCredentials, such as a token refresh guarded end to end.
func RotateSessionLocked(path, profile string, cookies map[string]string, expires time.Time) (Credentials, error) {
	disk, err := LoadCredentials(path)
	if err != nil {
		return Credentials{}, err
	}
	s, ok := disk.Profiles[profile]
	if !ok {
		return disk, nil
	}
	if s.Cookies == nil {
		s.Cookies = map[string]string{}
	}
	maps.Copy(s.Cookies, cookies)
	if !expires.IsZero() {
		s.SessionExpires = expires
	}
	s.UpdatedAt = time.Now().UTC()
	disk.Profiles[profile] = s
	if err := SaveCredentials(path, disk); err != nil {
		return Credentials{}, err
	}
	return disk, nil
}
