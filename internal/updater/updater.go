// Package updater checks GitHub releases and installs checksum-verified binaries.
package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// API is the latest-release endpoint. A variable so tests can point it at a
// fake GitHub.
var API = "https://api.github.com/repos/Microck/vinted-cli/releases/latest"

// ReleasesPage is where a person looks for releases.
const ReleasesPage = "https://github.com/Microck/vinted-cli/releases"

// ErrNoRelease means GitHub has no stable release to offer: releases/latest
// answers 404 until the first non-prerelease is published.
var ErrNoRelease = errors.New("no stable release of vinted-cli is published yet")

// statusError is a non-200 answer from GitHub.
type statusError struct{ status int }

func (e *statusError) Error() string { return fmt.Sprintf("update request: HTTP %d", e.status) }

const downloadBase = "https://github.com/Microck/vinted-cli/releases/download/"
const maxArchive = 128 << 20

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

func Newer(latest, current string) bool {
	l, c := versionPattern.FindStringSubmatch(latest), versionPattern.FindStringSubmatch(current)
	if l == nil || c == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		a, errA := strconv.ParseUint(l[i], 10, 64)
		b, errB := strconv.ParseUint(c[i], 10, 64)
		if errA != nil || errB != nil {
			return false
		}
		if a != b {
			return a > b
		}
	}
	return false
}

func Stable(version string) bool { return versionPattern.MatchString(version) }

func get(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "vinted-cli-updater")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &statusError{status: res.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("update download exceeds size limit")
	}
	return data, nil
}

func Latest(ctx context.Context, client *http.Client) (Release, error) {
	data, err := get(ctx, client, API, 1<<20)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return Release{}, ErrNoRelease
	}
	if err != nil {
		return Release{}, err
	}
	var r Release
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if !Stable(r.Tag) || r.Draft || r.Prerelease {
		return r, fmt.Errorf("release is not a stable version")
	}
	return r, nil
}

// Manager returns the package manager command for a managed executable.
func Manager(executable string) []string {
	p := strings.ToLower(filepath.ToSlash(executable))
	switch {
	case strings.Contains(p, "/node_modules/vinted-cli/"):
		return []string{"npm", "install", "-g", "vinted-cli@latest"}
	case strings.Contains(p, "/caskroom/vinted/") || strings.Contains(p, "/cellar/vinted/"):
		return []string{"brew", "upgrade", "--cask", "Microck/tap/vinted"}
	case strings.Contains(p, "/scoop/apps/vinted/"):
		return []string{"scoop", "update", "vinted"}
	case p == "/usr/bin/vinted":
		return []string{"yay", "-S", "vinted-cli-bin"}
	}
	return nil
}

func Install(ctx context.Context, client *http.Client, r Release, executable string) error {
	if !Stable(r.Tag) || r.Draft || r.Prerelease {
		return fmt.Errorf("invalid stable release")
	}
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows") || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("no release binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	name := fmt.Sprintf("vinted_%s_%s_%s%s", strings.TrimPrefix(r.Tag, "v"), runtime.GOOS, runtime.GOARCH, ext)
	base := downloadBase + r.Tag + "/"
	sums, err := get(ctx, client, base+"checksums.txt", 1<<20)
	if err != nil {
		return err
	}
	archive, err := get(ctx, client, base+name, maxArchive)
	if err != nil {
		return err
	}
	if err = Verify(archive, sums, name); err != nil {
		return err
	}
	binary, err := Extract(archive, ext)
	if err != nil {
		return err
	}
	return Replace(executable, binary)
}

func Verify(data, sums []byte, name string) error {
	digest := sha256.Sum256(data)
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if fields[0] != hex.EncodeToString(digest[:]) {
				return fmt.Errorf("checksum mismatch for %s", name)
			}
			return nil
		}
	}
	return fmt.Errorf("checksum missing for %s", name)
}

// Extract reads only the expected executable; archive paths are never written.
func Extract(data []byte, ext string) ([]byte, error) {
	name := "vinted"
	if ext == ".zip" {
		name += ".exe"
	}
	read := func(r io.Reader) ([]byte, error) {
		b, err := io.ReadAll(io.LimitReader(r, maxArchive+1))
		if err != nil {
			return nil, err
		}
		if len(b) == 0 || len(b) > maxArchive {
			return nil, fmt.Errorf("invalid executable size")
		}
		return b, nil
	}
	if ext == ".zip" {
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range z.File {
			if f.Name == name && f.Mode().IsRegular() {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return read(r)
			}
		}
	} else {
		g, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer g.Close()
		t := tar.NewReader(g)
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			if h.Name == name && h.Typeflag == tar.TypeReg {
				return read(t)
			}
		}
	}
	return nil, fmt.Errorf("archive has no %s executable", name)
}

func Replace(path string, binary []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".vinted-update-*")
	if err != nil {
		return fmt.Errorf("cannot write installation directory: %w", err)
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(binary); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(info.Mode().Perm()); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Rename(temp, path)
	}
	// Windows cannot replace a running executable. Keep it under a unique name.
	backup := temp + ".old"
	if err = os.Rename(path, backup); err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		if restore := os.Rename(backup, path); restore != nil {
			return fmt.Errorf("install failed: %v; restore failed: %v; previous binary: %s", err, restore, backup)
		}
		return err
	}
	_ = os.Remove(backup) // Running Windows binaries may remain until manually removed.
	return nil
}

func Client() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: transport}
}
