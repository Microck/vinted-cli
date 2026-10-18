package galleton

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Microck/vinted-cli/internal/galleton/bundle"
)

func privateDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("session state must use an absolute directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state is not a regular directory")
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		return errors.New("session directory must be private (mode 0700)")
	}
	return nil
}
func readRegular(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("expected a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("file too large")
	}
	return b, err
}
func atomicFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// cleanEnv keeps platform essentials and certificate trust settings, not
// provider cookies, proxy settings, or arbitrary app secrets.
func cleanEnv() []string {
	var env []string
	for _, key := range []string{"HOME", "USERPROFILE", "SystemRoot", "SYSTEMROOT", "WINDIR", "PATH", "TMPDIR", "TMP", "TEMP", "LOCALAPPDATA", "APPDATA", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func engineBinary(ctx context.Context, dir string) (string, error) {
	binDir := filepath.Join(dir, "bin")
	if err := privateDir(binDir); err != nil {
		return "", err
	}
	if err := atomicFile(filepath.Join(binDir, "LICENSE.galleton"), bundle.License, 0600); err != nil {
		return "", err
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	if len(bundle.Binary) > 0 {
		hash := sha256.Sum256(bundle.Binary)
		path := filepath.Join(binDir, fmt.Sprintf("galleton-%x%s", hash[:16], ext))
		old, err := readRegular(path, int64(len(bundle.Binary)))
		if err == nil && sha256.Sum256(old) == hash {
			return path, nil
		}
		if err := atomicFile(path, bundle.Binary, 0700); err != nil {
			return "", err
		}
		return path, nil
	}
	// go install builds cannot run go:generate. They use the already-required Go
	// toolchain once, with the same module version and checksums as release builds.
	path := filepath.Join(binDir, "galleton-"+bundle.Version+ext)
	if raw, err := readRegular(path, 32<<20); err == nil {
		expected, err := readRegular(path+".sha256", 128)
		if err == nil && strings.TrimSpace(string(expected)) == fmt.Sprintf("%x", sha256.Sum256(raw)) {
			return path, nil
		}
	}
	goExe, err := exec.LookPath("go")
	if err != nil {
		return "", errors.New("this source build needs Go once to prepare its session engine; release binaries and `make install` include it")
	}
	download := exec.CommandContext(ctx, goExe, "mod", "download", "-json", bundle.Module+"@"+bundle.Version)
	download.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	raw, err := download.Output()
	if err != nil {
		return "", errors.New("could not prepare the pinned session engine; use a release binary or run `make install`")
	}
	var mod struct{ Dir, Sum, GoModSum string }
	if json.Unmarshal(raw, &mod) != nil || mod.Sum != bundle.Sum || mod.GoModSum != bundle.ModSum {
		return "", errors.New("pinned Galleton checksum mismatch")
	}
	temp, err := os.MkdirTemp(binDir, ".build-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	src := filepath.Join(temp, "galleton"+ext)
	// Build the verified module directory directly: go install @version also
	// queries @latest for deprecation metadata, breaking offline cache reuse.
	cmd := exec.CommandContext(ctx, goExe, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", src, "./cmd/galleton")
	cmd.Dir = mod.Dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if err := cmd.Run(); err != nil {
		return "", errors.New("could not build the pinned session engine; use a release binary or run `make install`")
	}
	info, err := buildinfo.ReadFile(src)
	if err != nil || info.Main.Path != bundle.Module {
		return "", errors.New("unexpected Galleton build provenance")
	}
	built, err := readRegular(src, 32<<20)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(src, 0700); err != nil {
		return "", err
	}
	if err := os.Rename(src, path); err != nil {
		return "", err
	}
	if err := atomicFile(path+".sha256", []byte(fmt.Sprintf("%x\n", sha256.Sum256(built))), 0600); err != nil {
		return "", err
	}
	return path, nil
}
