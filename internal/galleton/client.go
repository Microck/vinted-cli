// Package galleton integrates the official Go SDK with vinted-cli's lifecycle.
// Cookie renewal and storage are exclusively implemented by the pinned daemon.
package galleton

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	sdk "github.com/Microck/galleton/client"
)

const DefaultURL = "http://127.0.0.1:8766"

// Configured selects an externally managed daemon. The default needs no settings.
func Configured() bool {
	return os.Getenv("VINTED_GALLETON_DIR") != "" || os.Getenv("VINTED_GALLETON_URL") != "" || os.Getenv("VINTED_GALLETON_TOKEN_FILE") != ""
}

type Client struct {
	sdk       *sdk.Client
	closeOnce sync.Once
	release   func()
}

// Error retains classification but never includes an upstream message or body.
type Error struct {
	Status  int
	Code    string
	RetryAt time.Time
}

func (e *Error) Error() string {
	return fmt.Sprintf("galleton returned HTTP %d (%s)", e.Status, e.Code)
}
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

type Metadata = sdk.Metadata
type Credentials = sdk.Credentials

// Response is one upstream answer relayed by the daemon. Set-Cookie never
// reaches it: the daemon keeps the cookies.
type Response = sdk.Response

// FromEnv is the advanced external-daemon path. It never installs or starts it.
func FromEnv() (*Client, error) {
	file := os.Getenv("VINTED_GALLETON_TOKEN_FILE")
	if file == "" {
		dir := os.Getenv("VINTED_GALLETON_DIR")
		if dir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				return nil, errors.New("cannot locate the external Galleton token")
			}
			dir = filepath.Join(base, "galleton")
		}
		file = filepath.Join(dir, "api.token")
	}
	token, err := readRegular(file, 4096)
	if err != nil {
		return nil, errors.New("cannot read the external Galleton api.token")
	}
	return New(os.Getenv("VINTED_GALLETON_URL"), string(token))
}

func New(base, token string) (*Client, error) {
	if base == "" {
		base = DefaultURL
	}
	u, err := url.Parse(base)
	if err != nil || u.ForceQuery {
		return nil, errors.New("galleton URL must be a plain loopback HTTP origin")
	}
	client, err := sdk.New(base, token)
	if err != nil {
		return nil, errors.New("galleton requires a literal loopback HTTP origin and a nonempty single-line API token")
	}
	return &Client{sdk: client}, nil
}

func safeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var e *sdk.Error
	if errors.As(err, &e) {
		out := &Error{Status: e.Status, Code: e.Code}
		if t, perr := time.Parse(time.RFC3339Nano, e.RetryAt); perr == nil {
			out.RetryAt = t
		}
		return out
	}
	return errors.New("galleton could not complete the operation; it may already have completed. Run `vinted auth service status` before retrying")
}
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
}
func (c *Client) Connect(ctx context.Context, id string, in Credentials) (Metadata, error) {
	out, err := c.sdk.Connect(ctx, id, in)
	return out, safeError(ctx, err)
}
func (c *Client) Status(ctx context.Context, id string) (Metadata, error) {
	out, err := c.sdk.Status(ctx, id)
	return out, safeError(ctx, err)
}
func (c *Client) Refresh(ctx context.Context, id string) (Metadata, error) {
	out, err := c.sdk.Refresh(ctx, id)
	return out, safeError(ctx, err)
}

// List is every session's metadata, the scheduler's last error included.
func (c *Client) List(ctx context.Context) ([]Metadata, error) {
	out, err := c.sdk.List(ctx)
	return out, safeError(ctx, err)
}

// Request sends one managed request: the daemon attaches the session's
// cookies, keeps whatever the response sets, and relays the answer. It is
// never replayed, here or in the daemon.
func (c *Client) Request(ctx context.Context, id, method, rawURL string, headers map[string]string, body []byte) (Response, error) {
	out, err := c.sdk.Request(ctx, id, method, rawURL, headers, body)
	return out, safeError(ctx, err)
}

// Pause stops managed requests and renewals of a provider for d (clamped to
// the daemon's 1 s to 24 h). It only ever extends an existing pause.
func (c *Client) Pause(ctx context.Context, provider string, d time.Duration) error {
	seconds := int((d + time.Second - 1) / time.Second)
	seconds = min(max(seconds, 1), 86400)
	_, err := c.sdk.Pause(ctx, provider, seconds)
	return safeError(ctx, err)
}

// Resume clears a provider pause.
func (c *Client) Resume(ctx context.Context, provider string) error {
	_, err := c.sdk.Resume(ctx, provider)
	return safeError(ctx, err)
}

func (c *Client) Forget(ctx context.Context, id string) error {
	err := safeError(ctx, c.sdk.Forget(ctx, id))
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
func (c *Client) Shutdown(ctx context.Context) error { return safeError(ctx, c.sdk.Shutdown(ctx)) }

// Ping is a read-only liveness check; it does not acquire or refresh credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.sdk.List(ctx)
	return safeError(ctx, err)
}
