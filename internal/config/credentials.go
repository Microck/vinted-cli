package config

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Microck/vinted-cli/internal/filelock"
)

func CloneCredentials(in Credentials) Credentials {
	return Credentials{Forget: slices.Clone(in.Forget), Profiles: maps.Clone(in.Profiles)}
}

// sameSession compares persisted timestamps by instant, not by time-zone
// pointers or monotonic clock data that may differ after a TOML round trip.
func sameSession(a, b Session) bool {
	return a.GalletonID == b.GalletonID && slices.Equal(a.Replaced, b.Replaced) && a.UserID == b.UserID && a.Login == b.Login &&
		a.Legacy == b.Legacy && a.UpdatedAt.Equal(b.UpdatedAt) && a.RefreshExpires.Equal(b.RefreshExpires) && a.RefreshIssued.Equal(b.RefreshIssued)
}

// LockCredentials serialises every read-modify-write of credentials.toml across
// processes, so two logins to different profiles cannot lose each other's
// write. The lock is not re-entrant. Waiting gives up after 30s or when ctx ends.
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
	// Pending deletions merge as a set: keep what other commands added, apply
	// only this command's own additions and removals.
	for _, id := range after.Forget {
		if !slices.Contains(before.Forget, id) && !slices.Contains(disk.Forget, id) {
			disk.Forget = append(disk.Forget, id)
		}
	}
	disk.Forget = slices.DeleteFunc(disk.Forget, func(id string) bool {
		return slices.Contains(before.Forget, id) && !slices.Contains(after.Forget, id)
	})
	if err := SaveCredentials(path, disk); err != nil {
		return Credentials{}, err
	}
	return disk, nil
}
