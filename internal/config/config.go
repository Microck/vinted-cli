// Package config owns everything the CLI persists outside the state database:
// XDG paths, config.toml (hand-edited, world-readable) and credentials.toml
// (which session-engine session each profile uses, 0600). The two files are kept apart so that sharing or committing
// a config never leaks a session.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/pelletier/go-toml/v2"
)

const appDir = "vinted-cli"

// Paths resolves where each file lives. VINTED_CONFIG moves only config.toml;
// credentials and state follow XDG so a relocated config cannot point the CLI
// at someone else's secrets.
type Paths struct {
	ConfigFile      string
	CredentialsFile string
	StateDB         string
	StateDir        string
}

func DefaultPaths() Paths {
	// adrg/xdg snapshots the environment at init; re-read it so XDG_* set by
	// the caller's environment (or a test) is honoured at call time.
	xdg.Reload()
	p := Paths{
		ConfigFile:      filepath.Join(xdg.ConfigHome, appDir, "config.toml"),
		CredentialsFile: filepath.Join(xdg.ConfigHome, appDir, "credentials.toml"),
		StateDB:         filepath.Join(xdg.DataHome, appDir, "state.db"),
		StateDir:        filepath.Join(xdg.StateHome, appDir),
	}
	if v := os.Getenv("VINTED_CONFIG"); v != "" {
		p.ConfigFile = v
	}
	return p
}

// ProfileConfig is the non-secret part of a profile. Domain picks the Vinted
// site (www.vinted.fr, www.vinted.es, ...); empty means VINTED_BASE_URL or
// www.vinted.com. It lives here, not in credentials, so anonymous profiles can
// search a national site too.
type ProfileConfig struct {
	Domain string `toml:"domain,omitempty" json:"domain,omitempty"`
}

// SinkConfig is one notification destination. Type selects which fields matter:
// ntfy uses URL+Topic(+TokenFile), webhook uses URL(+Template), exec uses Command.
type SinkConfig struct {
	Type      string   `toml:"type" json:"type"`
	URL       string   `toml:"url,omitempty" json:"url,omitempty"`
	Topic     string   `toml:"topic,omitempty" json:"topic,omitempty"`
	TokenFile string   `toml:"token_file,omitempty" json:"token_file,omitempty"`
	Template  string   `toml:"template,omitempty" json:"template,omitempty"`
	Command   []string `toml:"command,omitempty" json:"command,omitempty"`
}

type WatchConfig struct {
	Interval string `toml:"interval,omitempty" json:"interval,omitempty"`
}

type Config struct {
	DefaultProfile string `toml:"default_profile,omitempty" json:"default_profile,omitempty"`
	// RequestInterval is the minimum gap between two requests to a Vinted
	// site, shared by every process ("2s" when empty; floor 1s).
	RequestInterval string `toml:"request_interval,omitempty" json:"request_interval,omitempty"`
	// Proxy routes all Vinted traffic through http(s)/socks5(h) and never
	// falls back to a direct connection. VINTED_PROXY overrides it. No
	// credentials here: this file is world-readable.
	Proxy    string                   `toml:"proxy,omitempty" json:"proxy,omitempty"`
	Profiles map[string]ProfileConfig `toml:"profiles,omitempty" json:"profiles,omitempty"`
	Watch    WatchConfig              `toml:"watch,omitempty" json:"watch,omitempty"`
	Sinks    map[string]SinkConfig    `toml:"sinks,omitempty" json:"sinks,omitempty"`
}

// RequestGap parses request_interval; zero when it is unset.
func (c Config) RequestGap() (time.Duration, error) {
	if c.RequestInterval == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.RequestInterval)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 2s", c.RequestInterval)
	}
	return d, nil
}

// Load reads config.toml. A missing file is an empty config, not an error.
// Unknown keys are errors so typos surface instead of silently doing nothing.
func Load(path string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := toml.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	out, err := toml.Marshal(cfg)
	if err != nil {
		return err
	}
	return WriteAtomic(path, out, 0o644)
}

// Session is one profile's login. The cookies are not here: they live in the
// Galleton session engine's encrypted vault, which renews them and is the
// only party that ever holds them. GalletonID names the profile's session
// there; the rest is non-secret metadata recorded at login.
type Session struct {
	GalletonID string `toml:"galleton_id" json:"session_id"`
	// Replaced are engine sessions an earlier login replaced but could not
	// delete; the next command that reaches the engine deletes them.
	Replaced  []string  `toml:"replaced_galleton_ids,omitempty" json:"-"`
	UserID    int64     `toml:"user_id" json:"user_id"`
	Login     string    `toml:"login" json:"login"`
	UpdatedAt time.Time `toml:"updated_at" json:"updated_at"`
	// RefreshExpires is when the imported refresh_token_web cookie expires,
	// as of the login (zero when the export does not say). Not a secret.
	RefreshExpires time.Time `toml:"refresh_expires,omitempty" json:"refresh_expires,omitempty"`
	// RefreshIssued is when the refresh token RefreshExpires belongs to was
	// recorded: the login, then the engine's last renewal seen. Each renewal
	// issues a new token assumed to live as long, so RefreshExpires moves.
	RefreshIssued time.Time `toml:"refresh_issued,omitempty" json:"-"`
	// Legacy marks a profile still holding the cookie jar an older
	// vinted-cli kept in this file. It is never used: the profile needs
	// `vinted auth login`, and the next write of the file drops the cookies.
	Legacy bool `toml:"-" json:"-"`
}

type Credentials struct {
	// Forget are engine sessions of profiles that logout or profile remove
	// already dropped from this file but the engine has not deleted yet; the
	// next command that reaches the engine deletes them. Dropping the profile
	// first means the file never names a session that is already gone.
	Forget   []string           `toml:"forget_galleton_ids,omitempty"`
	Profiles map[string]Session `toml:"profiles"`
}

func LoadCredentials(path string) (Credentials, error) {
	creds := Credentials{Profiles: map[string]Session{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return creds, err
	}
	if err := toml.Unmarshal(raw, &creds); err != nil {
		return creds, fmt.Errorf("%s: %w", path, err)
	}
	if creds.Profiles == nil {
		creds.Profiles = map[string]Session{}
	}
	var legacy struct {
		Profiles map[string]struct {
			Cookies map[string]any `toml:"cookies"`
		} `toml:"profiles"`
	}
	if err := toml.Unmarshal(raw, &legacy); err == nil {
		for name, p := range legacy.Profiles {
			if s, ok := creds.Profiles[name]; ok && len(p.Cookies) > 0 && s.GalletonID == "" {
				s.Legacy = true
				creds.Profiles[name] = s
			}
		}
	}
	return creds, nil
}

func SaveCredentials(path string, creds Credentials) error {
	out, err := toml.Marshal(creds)
	if err != nil {
		return err
	}
	return WriteAtomic(path, out, 0o600)
}

// WriteAtomic writes to a sibling temp file and renames, so a crash never
// leaves a half-written file. The mode is applied before any bytes land, and
// again afterwards in case the file already existed.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return os.Chmod(path, mode)
}

// ResolveProfile picks the active profile name: flag, then env, then config
// default, then "default".
func ResolveProfile(flag string, cfg Config) string {
	switch {
	case flag != "":
		return flag
	case os.Getenv("VINTED_PROFILE") != "":
		return os.Getenv("VINTED_PROFILE")
	case cfg.DefaultProfile != "":
		return cfg.DefaultProfile
	}
	return "default"
}

// UserHome is the home directory, or "." if it cannot be determined.
func UserHome() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
