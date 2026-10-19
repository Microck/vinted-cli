package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdk "github.com/Microck/galleton/client"

	"github.com/Microck/vinted-cli/internal/cli"
	"github.com/Microck/vinted-cli/internal/testutil"
)

// engineClient connects to the session engine a command left running (it
// stays up for a short idle grace after the last command).
func (h *harness) engineClient() *sdk.Client {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.engineDir(), "endpoint.json"))
	if err != nil {
		h.t.Fatalf("the session engine is not running: %v", err)
	}
	var ep struct{ URL string }
	if err := json.Unmarshal(raw, &ep); err != nil {
		h.t.Fatal(err)
	}
	c, err := sdk.FromDir(ep.URL, h.engineDir())
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func (h *harness) pausedUntil() *time.Time {
	h.t.Helper()
	providers, err := h.engineClient().Providers(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range providers {
		if p.Name == "vinted-custom" {
			return p.PausedUntil
		}
	}
	h.t.Fatal("the engine has no provider for the fake site")
	return nil
}

// A block seen on an account request pauses the session engine too, so it
// renews nothing while the site blocks; clearing the cooldown lifts both.
func TestBlockPausesTheEngineUntilCooldownClear(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/api/v2/users/current", datadomeBlock(t))
	if r := h.run("", "me", "show"); r.code != 5 {
		t.Fatalf("blocked me show: exit %d\n%s", r.code, r.stderr)
	}
	until := h.pausedUntil()
	if until == nil || time.Until(*until) < 5*time.Hour {
		t.Fatalf("engine pause %v, want about 6h", until)
	}
	h.fake.setBlock("", nil)
	h.must("", "cooldown", "clear", "--yes")
	if until := h.pausedUntil(); until != nil {
		t.Fatalf("engine still paused until %v after cooldown clear", until)
	}
	h.must("", "me", "show")
}

// A cooldown on another custom site does not pause this site's engine
// provider: every custom host shares the provider name.
func TestOtherCustomSiteCooldownLeavesTheEngineRunning(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.home, "state", "vinted-cli", "throttle.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	st := `{"sites":{"127.0.0.2:1":{"blocked_until":"` + until + `","block_cause":"test"}}}`
	if err := os.WriteFile(path, []byte(st), 0o600); err != nil {
		t.Fatal(err)
	}
	h.login()
	if until := h.pausedUntil(); until != nil {
		t.Fatalf("engine paused until %v for another site's cooldown", until)
	}
	h.must("", "me", "show")
}

// engineUp reports a published engine endpoint without running a command.
func (h *harness) engineUp() bool {
	_, err := os.Stat(filepath.Join(h.engineDir(), "endpoint.json"))
	return err == nil
}

func (h *harness) engineRunning() bool {
	h.t.Helper()
	return decode[map[string]any](h.t, h.must("", "auth", "service", "status").stdout)["running"] == true
}

// The engine renews due sessions the moment it starts, before a pause can
// reach it, so during a cooldown a stopped engine stays stopped. Clearing the
// cooldown still works, and the next command starts it again.
func TestCooldownKeepsAStoppedEngineStopped(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/api/v2/users/current", datadomeBlock(t))
	if r := h.run("", "me", "show"); r.code != 5 {
		t.Fatalf("blocked me show: exit %d\n%s", r.code, r.stderr)
	}
	h.fake.setBlock("", nil)
	h.must("", "auth", "service", "stop")
	if r := h.run("", "me", "show"); r.code != 5 {
		t.Fatalf("me show during the cooldown: exit %d\n%s", r.code, r.stderr)
	}
	if h.engineRunning() {
		t.Fatal("a command started the session engine during a cooldown")
	}
	h.must("", "cooldown", "clear", "--yes")
	h.must("", "me", "show")
	if !h.engineRunning() {
		t.Fatal("the session engine did not start after the cooldown was cleared")
	}
}

// A request_interval that stops every request stops the engine's renewals
// too: no command starts the engine with it.
func TestBadRequestIntervalKeepsTheEngineStopped(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.must("", "auth", "service", "stop")
	cfg := filepath.Join(h.home, "config", "vinted-cli", "config.toml")
	if err := os.WriteFile(cfg, []byte("request_interval = \"10ms\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "me", "show"); r.code != 2 {
		t.Fatalf("me show with a bad interval: exit %d\n%s", r.code, r.stderr)
	}
	if h.engineRunning() {
		t.Fatal("the session engine started with a request_interval every request refuses")
	}
}

// One engine, one egress: a command with a different proxy never borrows a
// running engine that would renew through another route.
func TestEngineRefusesADifferentProxy(t *testing.T) {
	h := newHarness(t)
	h.login()
	t.Setenv("VINTED_PROXY", "socks5h://"+testutil.ClosedAddr(t))
	r := h.run("", "me", "show")
	if r.code == 0 || !strings.Contains(r.stderr, "different proxy") {
		t.Fatalf("attached to an engine with another egress: exit %d\n%s", r.code, r.stderr)
	}
}

func TestAuthServiceStatusAndStop(t *testing.T) {
	h := newHarness(t)
	st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout)
	if st["running"] != false || st["external"] != false {
		t.Fatalf("status before any login %v", st)
	}
	h.login()
	if st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout); st["running"] != true {
		t.Fatalf("status after login %v", st)
	}
	if out := decode[map[string]any](t, h.must("", "auth", "service", "stop").stdout); out["stopped"] != true {
		t.Fatalf("stop %v", out)
	}
	if st := decode[map[string]any](t, h.must("", "auth", "service", "status").stdout); st["running"] != false {
		t.Fatalf("status after stop %v", st)
	}
	// The vault outlives the engine: the next command restarts it.
	h.must("", "me", "show")
}

// A new login is imported beside the current session and replaces it only
// once it works: a rejected one leaves the profile on its old session, and a
// good one removes the old session from the engine.
func TestReloginReplacesTheSessionOnlyWhenTheNewOneWorks(t *testing.T) {
	h := newHarness(t)
	first := decode[map[string]any](t, h.login().stdout)["session_id"].(string)

	bad := filepath.Join(t.TempDir(), "spent.txt")
	if err := os.WriteFile(bad, []byte("access_token_web=x; refresh_token_web=spent-refresh-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.run("", "auth", "login", "--cookies", bad); r.code != 3 {
		t.Fatalf("login with a spent token: exit %d\n%s", r.code, r.stderr)
	}
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["session_id"] != first {
		t.Fatalf("a rejected login replaced the session: %v", st)
	}
	h.must("", "me", "show")

	second := decode[map[string]any](t, h.login().stdout)["session_id"].(string)
	if second == first {
		t.Fatal("a new login reused the old session id")
	}
	sessions, err := h.engineClient().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != second {
		t.Fatalf("engine sessions after relogin: %+v", sessions)
	}
	h.must("", "me", "show")
}

// A block on the engine's own renewal starts the shared cooldown and the
// engine pause, the same as a block the CLI saw on one of its requests.
func TestBlockedEngineRenewalStartsTheCooldown(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	r := h.run("", "auth", "refresh")
	if r.code != 5 || !strings.Contains(r.stderr, "renewal") {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("no cooldown after a blocked renewal: %v", cd)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("the engine was not paused after its renewal was blocked")
	}
}

// Datadome can answer a renewal with its challenge page and HTTP 200: that is
// a block, not a renewal that broke the session.
func TestEngineRenewalChallengeServedWith200IsABlock(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]any](t, h.login().stdout)["session_id"].(string)
	h.fake.setBlock("/web/api/auth/refresh", &fakeBlock{status: http.StatusOK, body: fixture(t, "datadome_403.html")})
	if r := h.run("", "auth", "refresh"); r.code != 5 {
		t.Fatalf("200 challenge on renewal: exit %d\n%s", r.code, r.stderr)
	}
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("no cooldown after a 200 challenge: %v", cd)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("the engine was not paused after a 200 challenge")
	}
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["session_id"] != id || st["logged_in"] != true {
		t.Fatalf("status after a 200 challenge: %v", st)
	}
}

// A renewal block that no command saw (the engine's own scheduler hit it)
// still starts the shared cooldown while `auth service run` watches it.
func TestAuthServiceRunReportsAnEngineRenewalBlock(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	if r := h.run("", "auth", "refresh"); r.code != 5 {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	// Forget the cooldown the command recorded: the engine still holds the
	// session in its blocked back-off, as after a scheduled renewal.
	h.must("", "cooldown", "clear", "--yes")
	stop := make(chan struct{})
	t.Cleanup(cli.UseServiceLoop(50*time.Millisecond, stop))
	time.AfterFunc(time.Second, func() { close(stop) })
	h.must("", "auth", "service", "run")
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("auth service run did not record the engine's renewal block: %v", cd)
	}
}

// The session keepalive of a scheduled `watch check` reports a renewal block
// the engine hit on its own, like `auth service run` does.
func TestWatchKeepaliveReportsAnEngineRenewalBlock(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	if r := h.run("", "auth", "refresh"); r.code != 5 {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	h.must("", "cooldown", "clear", "--yes")
	h.run("", "watch", "check")
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("watch check did not record the engine's renewal block: %v", cd)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("watch check recorded the block but left the engine unpaused")
	}
}

// A running MCP server reports a renewal block the engine hit on its own
// between tool calls.
func TestMCPServerReportsAnEngineRenewalBlock(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	if r := h.run("", "auth", "refresh"); r.code != 5 {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	h.must("", "cooldown", "clear", "--yes")
	t.Cleanup(cli.UseServiceLoop(50*time.Millisecond, nil))
	s := h.mcpServer()
	deadline := time.Now().Add(30 * time.Second)
	// Only the server runs a command now: read the state files directly.
	for h.throttleState()[h.site()]["blocked_until"] == nil {
		if time.Now().After(deadline) {
			t.Fatal("the mcp server did not record the engine's renewal block")
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.close()
}

// `auth service run` started during a cooldown waits for it to end instead
// of exiting, then starts the engine.
func TestAuthServiceRunWaitsOutACooldown(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/api/v2/users/current", datadomeBlock(t))
	if r := h.run("", "me", "show"); r.code != 5 {
		t.Fatalf("blocked me show: exit %d\n%s", r.code, r.stderr)
	}
	h.fake.setBlock("", nil)
	h.must("", "auth", "service", "stop")
	stop := make(chan struct{})
	t.Cleanup(cli.UseServiceLoop(50*time.Millisecond, stop))
	done := make(chan result, 1)
	go func() { done <- h.run("", "auth", "service", "run") }()
	time.Sleep(500 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("auth service run exited during the cooldown: exit %d\n%s", r.code, r.stderr)
	default:
	}
	// The service command is running: check the files, not other commands.
	if h.engineUp() {
		t.Fatal("auth service run started the engine during a cooldown")
	}
	if err := os.Remove(filepath.Join(h.home, "state", "vinted-cli", "throttle.json")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for !h.engineUp() {
		if time.Now().After(deadline) {
			t.Fatal("auth service run did not start the engine after the cooldown")
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(stop)
	if r := <-done; r.code != 0 {
		t.Fatalf("auth service run: exit %d\n%s", r.code, r.stderr)
	}
}

// A block seen by a logged-out profile still pauses the running engine,
// which renews another profile's session for the same site.
func TestGuestBlockPausesARunningEngine(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.setBlock("/catalog", datadomeBlock(t))
	if r := h.run("", "search", "nike", "--profile", "guest"); r.code != 5 {
		t.Fatalf("blocked guest search: exit %d\n%s", r.code, r.stderr)
	}
	if until := h.pausedUntil(); until == nil {
		t.Fatal("a guest block left the running engine renewing")
	}
}

// chat open checks for a renewal the engine ran on its own and the bot
// protection blocked between polls, and stops with exit 5 on it.
func TestChatOpenReportsAnEngineRenewalBlockBetweenPolls(t *testing.T) {
	h, _ := newSellerHarness(t)
	h.fake.setBlock("/web/api/auth/refresh", datadomeBlock(t))
	if r := h.run("", "auth", "refresh"); r.code != 5 {
		t.Fatalf("blocked renewal: exit %d\n%s", r.code, r.stderr)
	}
	// Forget the cooldown the command recorded and lift the block: the engine
	// still holds the session in its blocked back-off, as after a scheduled
	// renewal, and would keep serving polls with the old credentials.
	h.must("", "cooldown", "clear", "--yes")
	h.fake.setBlock("", nil)
	t.Cleanup(cli.UseChatPoll(20 * time.Millisecond))
	stdin, w := io.Pipe()
	defer w.Close()
	var out, errb bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cli.Execute("test", []string{"chat", "open", fmt.Sprint(buyerConvID), "--interval", "20ms"}, stdin, &out, &errb)
	}()
	select {
	case code := <-done:
		if code != 5 {
			t.Fatalf("chat open: exit %d\n%s", code, errb.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("chat open kept polling through the engine's blocked renewal")
	}
	if cd := decode[map[string]any](t, h.must("", "cooldown").stdout); cd["blocked"] != true {
		t.Fatalf("chat open did not record the engine's renewal block: %v", cd)
	}
}

// logout writes credentials.toml before the engine deletes the session, so a
// failed write leaves the login usable and a retry works.
func TestLogoutKeepsTheSessionWhenTheFileCannotBeWritten(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs unix permissions that apply to the test user")
	}
	h := newHarness(t)
	id := decode[map[string]any](t, h.login().stdout)["session_id"].(string)
	dir := filepath.Dir(h.credentialsFile())
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	r := h.run("", "auth", "logout")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if r.code == 0 {
		t.Fatal("logout succeeded without writing credentials.toml")
	}
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["session_id"] != id {
		t.Fatalf("status after a failed logout: %v", st)
	}
	h.must("", "me", "show")
	h.must("", "auth", "logout")
	if sessions, err := h.engineClient().List(context.Background()); err != nil || len(sessions) != 0 {
		t.Fatalf("engine sessions after logout: %+v %v", sessions, err)
	}
}

// A session logout dropped from credentials.toml but the engine did not
// delete yet is deleted by the next command that reaches the engine.
func TestPendingSessionDeletionIsRetried(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]any](t, h.login().stdout)["session_id"].(string)
	if err := os.WriteFile(h.credentialsFile(), []byte("forget_galleton_ids = ['"+id+"']\n[profiles]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.must("", "auth", "logout")
	if sessions, err := h.engineClient().List(context.Background()); err != nil || len(sessions) != 0 {
		t.Fatalf("engine sessions after the retried deletion: %+v %v", sessions, err)
	}
	raw, err := os.ReadFile(h.credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), id) {
		t.Fatalf("credentials.toml still queues the deleted session:\n%s", raw)
	}
}
