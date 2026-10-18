// Package sink delivers events somewhere other than stdout: ntfy, a JSON
// webhook, or a local executable. Sinks are declared in config.toml.
package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/store"
)

// Deliver sends one event to one sink. Errors are returned, not fatal: a dead
// webhook must not stop the other sinks or the state commit.
func Deliver(ctx context.Context, name string, cfg config.SinkConfig, ev store.Event) error {
	switch cfg.Type {
	case "ntfy":
		return ntfy(ctx, cfg, ev)
	case "webhook":
		return webhook(ctx, cfg, ev)
	case "exec":
		return execSink(ctx, cfg, ev)
	}
	return fmt.Errorf("sink %q has unknown type %q (want ntfy, webhook or exec)", name, cfg.Type)
}

// Validate checks a sink config without sending anything.
func Validate(name string, cfg config.SinkConfig) error {
	if err := CheckValues(name, cfg); err != nil {
		return err
	}
	switch cfg.Type {
	case "ntfy":
		if cfg.URL == "" || cfg.Topic == "" {
			return fmt.Errorf("sink %q: ntfy needs url and topic", name)
		}
	case "webhook":
		if cfg.URL == "" {
			return fmt.Errorf("sink %q: webhook needs url", name)
		}
	case "exec":
		if len(cfg.Command) == 0 {
			return fmt.Errorf("sink %q: exec needs command", name)
		}
	default:
		return fmt.Errorf("sink %q has unknown type %q (want ntfy, webhook or exec)", name, cfg.Type)
	}
	return nil
}

// CheckValues checks the fields that take one of a fixed set of values, and
// nothing else, so a sink being set up one key at a time can be checked after
// every key: a typo in type or template is caught when it is written, not at
// the first event. An unset type passes here; Validate requires one.
func CheckValues(name string, cfg config.SinkConfig) error {
	switch cfg.Type {
	case "", "ntfy", "webhook", "exec":
	default:
		return fmt.Errorf("sink %q has unknown type %q (want ntfy, webhook or exec)", name, cfg.Type)
	}
	switch cfg.Template {
	case "", "discord", "slack":
	default:
		return fmt.Errorf("sink %q has unknown template %q (want discord, slack, or none for the raw event)", name, cfg.Template)
	}
	return nil
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Summary is the one-line human text used by ntfy and chat webhooks.
func Summary(ev store.Event) (title, body, link string) {
	var item struct {
		Title    string  `json:"title"`
		Price    float64 `json:"price"`
		Currency string  `json:"currency"`
		URL      string  `json:"url"`
	}
	_ = json.Unmarshal(ev.Item, &item)
	var change struct {
		From any `json:"from"`
		To   any `json:"to"`
	}
	_ = json.Unmarshal(ev.Change, &change)
	price := fmt.Sprintf("%.0f %s", item.Price, item.Currency)
	switch ev.Type {
	case "item.new", "seller.new_item":
		title = "New: " + item.Title
		body = price
	case "item.price_changed":
		title = "Price: " + item.Title
		body = fmt.Sprintf("%v -> %v %s", change.From, change.To, item.Currency)
	case "item.reserved":
		title = "Reserved: " + item.Title
		body = price
	case "item.unreserved":
		title = "Available again: " + item.Title
		body = price
	case "item.sold":
		title = "Sold: " + item.Title
	case "item.removed":
		title = "Removed: " + item.Title
	default:
		title = ev.Type + ": " + item.Title
	}
	return title, strings.TrimSpace(body + " [" + ev.Watch + "]"), item.URL
}

func ntfy(ctx context.Context, cfg config.SinkConfig, ev store.Event) error {
	title, body, link := Summary(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.URL, "/")+"/"+cfg.Topic, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "shopping_cart")
	if link != "" {
		req.Header.Set("Click", link)
	}
	if cfg.TokenFile != "" {
		tok, err := os.ReadFile(expandHome(cfg.TokenFile))
		if err != nil {
			return fmt.Errorf("ntfy token file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	return doHTTP(req)
}

func webhook(ctx context.Context, cfg config.SinkConfig, ev store.Event) error {
	var payload any = ev
	// Discord and Slack want a text field; everything else gets the raw event.
	switch cfg.Template {
	case "discord":
		title, body, link := Summary(ev)
		payload = map[string]string{"content": title + "\n" + body + "\n" + link}
	case "slack":
		title, body, link := Summary(ev)
		payload = map[string]string{"text": title + "\n" + body + "\n" + link}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doHTTP(req)
}

func doHTTP(req *http.Request) error {
	req.Header.Set("User-Agent", "vinted-cli")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered HTTP %d", req.URL.Host, resp.StatusCode)
	}
	return nil
}

func execSink(ctx context.Context, cfg config.SinkConfig, ev store.Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := make([]string, len(cfg.Command))
	for i, a := range cfg.Command {
		args[i] = expandHome(a)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Env = append(os.Environ(), "VINTED_EVENT_TYPE="+ev.Type, "VINTED_WATCH="+ev.Watch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
