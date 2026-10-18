package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/vinted-cli/internal/cli"
)

// harness drives the CLI through its entrypoint against a fake Vinted with
// an isolated XDG home. Every test goes through here; the only reach into
// package internals is the request gate's clock, which runs fake so pacing
// costs no real time and can be measured.
type harness struct {
	t     *testing.T
	fake  *fakeVinted
	home  string
	clock *fakeClock
}

// fakeClock advances only when the gate sleeps on it, and records the moment
// each sleep ends: when the gated request went out.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	sends []time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d > 0 {
		c.now = c.now.Add(d)
	}
	c.sends = append(c.sends, c.now)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) sent() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.sends...)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeVinted(t)
	home := t.TempDir()
	clock := &fakeClock{now: time.Now()}
	t.Cleanup(cli.UseThrottleClock(clock))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("VINTED_PROXY", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HOME", home)
	t.Setenv("VINTED_PROFILE", "")
	t.Setenv("VINTED_COOKIES", "")
	t.Setenv("VINTED_CONFIG", "")
	t.Setenv("VINTED_BASE_URL", fake.srv.URL)
	t.Setenv("VINTED_NO_UPDATE_CHECK", "1")
	t.Setenv("NO_COLOR", "")
	return &harness{t: t, fake: fake, home: home, clock: clock}
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (h *harness) run(stdin string, args ...string) result {
	h.t.Helper()
	var out, errb bytes.Buffer
	code := cli.Execute("test", args, strings.NewReader(stdin), &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (h *harness) must(stdin string, args ...string) result {
	h.t.Helper()
	r := h.run(stdin, args...)
	if r.code != 0 {
		h.t.Fatalf("vinted %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	}
	return r
}

// login imports the fake's cookie export into the default profile.
func (h *harness) login(args ...string) result {
	h.t.Helper()
	return h.must("", append([]string{"auth", "login", "--cookies", h.fake.cookieFile(h.t)}, args...)...)
}

func (h *harness) credentialsFile() string {
	return filepath.Join(h.home, "config", "vinted-cli", "credentials.toml")
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("output is not the expected json: %v\n%s", err, raw)
	}
	return v
}

func decodeInto(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("output is not the expected json: %v\n%s", err, raw)
	}
}

type itemsOut struct {
	Items []struct {
		ID       int64   `json:"id"`
		Title    string  `json:"title"`
		Price    float64 `json:"price"`
		Reserved bool    `json:"reserved"`
		URL      string  `json:"url"`
	} `json:"items"`
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

type eventOut struct {
	Type  string `json:"type"`
	Watch string `json:"watch"`
	Item  struct {
		ID    int64   `json:"id"`
		Price float64 `json:"price"`
	} `json:"item"`
	Change map[string]any `json:"change"`
}

func TestLoginImportsCookiesRefreshesAndProtectsCredentials(t *testing.T) {
	h := newHarness(t)
	r := h.login("--profile", "test")
	st := decode[map[string]any](t, r.stdout)
	if st["logged_in"] != true || st["profile"] != "test" || st["account"] != fixtureLogin || st["user_id"] != float64(fakeUserID) {
		t.Fatalf("login status %v", st)
	}
	if st["domain"] != "www.vinted.fr" {
		t.Errorf("site not taken from the cookie domain: %v", st["domain"])
	}
	// The exported access token had expired, so login refreshed it, and the
	// renewed token is what landed on disk.
	if h.fake.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", h.fake.refreshes)
	}
	info, err := os.Stat(h.credentialsFile())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode %v, want 0600", info.Mode().Perm())
	}
	raw, _ := os.ReadFile(h.credentialsFile())
	if !strings.Contains(string(raw), h.fake.access) {
		t.Fatal("the refreshed access token was not persisted")
	}
	if strings.Contains(r.stdout+r.stderr, fakeRefresh) || strings.Contains(r.stdout+r.stderr, h.fake.access) {
		t.Fatal("login printed a cookie value")
	}
	cfg, _ := os.ReadFile(filepath.Join(h.home, "config", "vinted-cli", "config.toml"))
	if strings.Contains(string(cfg), fakeRefresh) || !strings.Contains(string(cfg), `default_profile = 'test'`) {
		t.Fatalf("config must name the first profile default and hold no secrets:\n%s", cfg)
	}
	// The first profile became the default, so no --profile is needed.
	me := decode[map[string]any](t, h.must("", "me", "show").stdout)
	if me["login"] != fixtureLogin {
		t.Fatalf("me show %v", me)
	}
}

func TestLoginRejectsRevokedSessionWithAuthExit(t *testing.T) {
	h := newHarness(t)
	h.fake.revoked = true
	r := h.run("", "auth", "login", "--cookies", h.fake.cookieFile(t))
	if r.code != 3 || !strings.Contains(r.stderr, "auth login") {
		t.Fatalf("revoked session: exit %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(h.credentialsFile()); err == nil {
		t.Fatal("a rejected login must not be saved")
	}
}

func TestLoginInputErrorsAreUsageErrors(t *testing.T) {
	h := newHarness(t)
	if r := h.run("", "auth", "login"); r.code != 2 || !strings.Contains(r.stderr, "--cookies") {
		t.Fatalf("no input: exit %d %q", r.code, r.stderr)
	}
	if r := h.run("foo=bar; baz=qux", "auth", "login", "--cookies-stdin"); r.code != 2 || !strings.Contains(r.stderr, "access_token_web") {
		t.Fatalf("cookies without tokens: exit %d %q", r.code, r.stderr)
	}
}

func TestStatusRefreshAndLogout(t *testing.T) {
	h := newHarness(t)
	if st := decode[map[string]any](t, h.must("", "auth", "status").stdout); st["logged_in"] != false {
		t.Fatalf("fresh status %v", st)
	}
	h.login()
	st := decode[map[string]any](t, h.must("", "auth", "status", "--check").stdout)
	if st["logged_in"] != true || st["session_valid"] != true || st["session_expires"] == nil {
		t.Fatalf("status %v", st)
	}
	before := h.fake.refreshes
	h.must("", "auth", "refresh")
	if h.fake.refreshes != before+1 {
		t.Fatal("auth refresh did not call the refresh endpoint")
	}
	h.must("", "auth", "logout")
	if r := h.run("", "me", "show"); r.code != 3 {
		t.Fatalf("me show after logout: exit %d", r.code)
	}
	list := decode[[]map[string]any](t, h.must("", "profile", "list").stdout)
	if len(list) != 0 {
		t.Fatalf("profiles after logout %v", list)
	}
}

// A token the server stopped accepting is refreshed on the 401 and the new
// one is written back, so the next process starts with it.
func TestServerSideExpiryRefreshesAndPersists(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.mu.Lock()
	old := h.fake.access
	h.fake.access = "" // the server forgets every token it handed out
	h.fake.mu.Unlock()
	// The 401 triggers a refresh, which mints a token the server accepts.
	h.must("", "me", "show")
	raw, _ := os.ReadFile(h.credentialsFile())
	if strings.Contains(string(raw), old) || !strings.Contains(string(raw), h.fake.access) {
		t.Fatal("the refreshed token was not persisted")
	}
}

func TestEnvCookiesAreUsedButNeverPersisted(t *testing.T) {
	h := newHarness(t)
	h.fake.mu.Lock()
	h.fake.access = jwtFor(t)
	tok := h.fake.access
	h.fake.mu.Unlock()
	t.Setenv("VINTED_COOKIES", "access_token_web="+tok+"; refresh_token_web="+fakeRefresh)
	me := decode[map[string]any](t, h.must("", "me", "show").stdout)
	if me["login"] != fixtureLogin {
		t.Fatalf("me show %v", me)
	}
	st := decode[map[string]any](t, h.must("", "auth", "status").stdout)
	if st["source"] != "env VINTED_COOKIES" {
		t.Fatalf("status source %v", st["source"])
	}
	if _, err := os.Stat(h.credentialsFile()); err == nil {
		t.Fatal("env cookies were written to disk")
	}
}

func TestDebugRedactsCookies(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.fake.mu.Lock()
	h.fake.access = ""
	h.fake.mu.Unlock()
	r := h.must("", "--debug", "me", "show")
	if !strings.Contains(r.stderr, "/api/v2/users/current") {
		t.Fatalf("debug log missing requests: %q", r.stderr)
	}
	for _, secret := range []string{fakeRefresh, h.fake.access, "datadome-cookie-value"} {
		if strings.Contains(r.stderr, secret) {
			t.Fatal("debug output leaked a cookie value")
		}
	}
}

func TestCommandsNeedingASessionExitThree(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{{"me", "show"}, {"me", "favorites"}, {"chat", "list"}, {"item", "favorite", "1"}} {
		r := h.run("", args...)
		if r.code != 3 || !strings.Contains(r.stderr, "vinted auth login") {
			t.Errorf("%v: exit %d %q", args, r.code, r.stderr)
		}
	}
}

func TestSearchIsAnonymousAndPassesFilters(t *testing.T) {
	h := newHarness(t)
	out := decode[itemsOut](t, h.must("", "search", "nike", "air", "--price-to", "30", "--order", "newest_first", "--catalog", "1206", "--brand", "53", "--condition", "good", "--filter", "material_ids[]=44").stdout)
	if len(out.Items) < 2 {
		t.Fatalf("search returned %d items", len(out.Items))
	}
	for _, it := range out.Items {
		if it.ID == 0 || it.Title == "" || it.URL == "" {
			t.Fatalf("incomplete item %+v", it)
		}
	}
	reqs := h.fake.requestsTo("/catalog")
	if len(reqs) != 1 {
		t.Fatalf("catalog requests %d", len(reqs))
	}
	q := reqs[0].Query
	if q.Get("search_text") != "nike air" || q.Get("order") != "newest_first" || q.Get("material_ids[]") != "44" {
		t.Fatalf("query %v", q)
	}
	if !strings.Contains(q.Encode(), "1206") || !strings.Contains(q.Encode(), "53") || !strings.Contains(q.Encode(), "=3") {
		t.Fatalf("catalog, brand or condition missing from %v", q)
	}
	limited := decode[itemsOut](t, h.must("", "search", "nike", "--limit", "1").stdout)
	if len(limited.Items) != 1 {
		t.Fatalf("--limit 1 returned %d", len(limited.Items))
	}
}

func TestSearchRejectsBadFlagsWithUsageExit(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"search", "x", "--order", "cheapest"},
		{"search", "x", "--condition", "mint"},
		{"search", "x", "--price-from", "50", "--price-to", "10"},
		{"search", "x", "--page", "0"},
		{"search", "x", "--format", "xml"},
		{"search", "x", "--bogus"},
	} {
		if r := h.run("", args...); r.code != 2 {
			t.Errorf("%v: exit %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}
	if len(h.fake.requestsTo("/catalog")) != 0 {
		t.Fatal("invalid flags must fail before any request")
	}
}

func TestOutputFormats(t *testing.T) {
	h := newHarness(t)
	// jsonl prints one value per line; a search page is one value. Lists such
	// as watch events stream one element per line (see the watch tests).
	jsonl := strings.TrimSpace(h.must("", "search", "nike", "--format", "jsonl").stdout)
	if strings.Contains(jsonl, "\n") || len(decode[itemsOut](t, jsonl).Items) < 2 {
		t.Fatalf("jsonl search page: %q", jsonl)
	}
	pretty := h.must("", "search", "nike", "--format", "pretty").stdout
	if !strings.Contains(pretty, "PRICE") || !strings.Contains(pretty, "TITLE") {
		t.Fatalf("pretty table missing headers:\n%s", pretty)
	}
	if toon := h.must("", "search", "nike", "--format", "toon").stdout; !strings.Contains(toon, "items") {
		t.Fatalf("toon output %q", toon)
	}
}

func TestItemShowImagesAndNotFound(t *testing.T) {
	h := newHarness(t)
	for _, ref := range []string{fixtureItemRef(), "https://www.vinted.fr/items/" + fixtureItemRef() + "-slug?referrer=catalog"} {
		it := decode[map[string]any](t, h.must("", "item", "show", ref).stdout)
		if it["id"] != float64(fixtureItemID) || it["title"] == "" || it["seller_id"] == nil {
			t.Fatalf("item show %s: %v", ref, it)
		}
	}
	imgs := decode[[]string](t, h.must("", "item", "images", fixtureItemRef()).stdout)
	if len(imgs) == 0 || !strings.HasPrefix(imgs[0], "http") {
		t.Fatalf("images %v", imgs)
	}
	if r := h.run("", "item", "show", "1"); r.code != 4 {
		t.Fatalf("missing item: exit %d %s", r.code, r.stderr)
	}
	if r := h.run("", "item", "show", "not-an-item"); r.code != 2 {
		t.Fatalf("bad ref: exit %d %s", r.code, r.stderr)
	}
}

func TestFavoriteTogglesOnlyWhenTheStateDiffers(t *testing.T) {
	h := newHarness(t)
	h.login()
	r := decode[map[string]any](t, h.must("", "item", "unfavorite", fixtureItemRef()).stdout)
	if r["action"] != "unfavorited" || r["ok"] != true {
		t.Fatalf("unfavorite %v", r)
	}
	h.must("", "item", "favorite", fixtureItemRef())
	// The fixture item is already favorited: unfavorite toggles, favorite is a no-op.
	if len(h.fake.toggles) != 1 || !strings.Contains(h.fake.toggles[0], fixtureItemRef()) {
		t.Fatalf("toggles %v", h.fake.toggles)
	}
}

// Vinted only offers a toggle, so an item page that does not state the
// current favourite state must not be toggled blind.
func TestFavoriteRefusesUnknownState(t *testing.T) {
	h := newHarness(t)
	h.login()
	for _, repl := range []string{`\"is_favourite\":null,`, ``} {
		h.fake.mu.Lock()
		h.fake.itemPage = bytes.ReplaceAll(fixture(t, "item.html"), []byte(`\"is_favourite\":true,`), []byte(repl))
		h.fake.mu.Unlock()
		for _, verb := range []string{"favorite", "unfavorite"} {
			r := h.run("", "item", verb, fixtureItemRef())
			if r.code != 7 || !strings.Contains(r.stderr, "favorites") {
				t.Fatalf("%s with is_favourite replaced by %q: exit %d %s", verb, repl, r.code, r.stderr)
			}
		}
	}
	if len(h.fake.toggles) != 0 {
		t.Fatalf("toggled blind: %v", h.fake.toggles)
	}
}

func TestUserMeAndCatalogLookups(t *testing.T) {
	h := newHarness(t)
	u := decode[map[string]any](t, h.must("", "user", "show", "https://www.vinted.fr/member/123456789-someone").stdout)
	if u["id"] == nil || u["login"] == "" {
		t.Fatalf("user show %v", u)
	}
	items := decode[itemsOut](t, h.must("", "user", "items", "123456789", "--page", "1", "--limit", "5").stdout)
	if len(items.Items) == 0 {
		t.Fatal("user items empty")
	}
	if q := h.fake.requestsTo("/api/v2/wardrobe/123456789/items")[0].Query; q.Get("per_page") != "5" {
		t.Fatalf("wardrobe query %v", q)
	}
	reviews := decode[[]map[string]any](t, h.must("", "user", "reviews", "123456789").stdout)
	if len(reviews) == 0 || reviews[0]["rating"] == nil {
		t.Fatalf("reviews %v", reviews)
	}
	brands := decode[[]map[string]any](t, h.must("", "brand", "search", "levi").stdout)
	if len(brands) == 0 || brands[0]["id"] == nil {
		t.Fatalf("brands %v", brands)
	}
	cats := decode[[]map[string]any](t, h.must("", "category", "list").stdout)
	if len(cats) == 0 || cats[0]["title"] == "" {
		t.Fatalf("categories %v", cats)
	}
	if pretty := h.must("", "category", "list", "--format", "pretty").stdout; !strings.Contains(pretty, "CATEGORY") {
		t.Fatalf("category pretty:\n%s", pretty)
	}
	h.login()
	h.must("", "me", "items")
	favs := decode[itemsOut](t, h.must("", "me", "favorites").stdout)
	if favs.Items == nil {
		t.Fatal("favorites must be a list")
	}
}

func TestChatListShowAndSend(t *testing.T) {
	h := newHarness(t)
	h.login()
	convs := decode[[]map[string]any](t, h.must("", "chat", "list").stdout)
	if len(convs) == 0 || convs[0]["id"] == nil {
		t.Fatalf("chat list %v", convs)
	}
	id := jsonID(convs[0]["id"])
	conv := decode[map[string]any](t, h.must("", "chat", "show", id).stdout)
	if msgs, _ := conv["messages"].([]any); len(msgs) == 0 {
		t.Fatalf("chat show %v", conv)
	}
	sent := decode[map[string]any](t, h.must("is it still available?\n", "chat", "send", id, "-").stdout)
	if sent["text"] != "is it still available?" {
		t.Fatalf("sent %v", sent)
	}
	if len(h.fake.replies) != 1 || !strings.Contains(h.fake.replies[0], "is it still available?") {
		t.Fatalf("replies %v", h.fake.replies)
	}
	if r := h.run("", "chat", "show", "abc"); r.code != 2 {
		t.Fatalf("bad conversation id: exit %d", r.code)
	}
}

func TestSellerWatchReportsEveryChange(t *testing.T) {
	h := newHarness(t)
	added := decode[map[string]any](t, h.must("", "watch", "add", "seller", "123456789", "--name", "seller").stdout)
	if added["baselined"] != true || added["seen"] == nil {
		t.Fatalf("watch add %v", added)
	}
	if out := strings.TrimSpace(h.must("", "watch", "check", "--all", "--format", "jsonl").stdout); out != "" {
		t.Fatalf("an unchanged seller produced events: %s", out)
	}
	first, second := wardrobeIDs(t, h)
	h.fake.setWardrobeItem(first, wardrobePriceField, wardrobePrice(1.5))
	h.fake.setWardrobeItem(first, "is_reserved", true)
	h.fake.removeWardrobeItem(second)
	h.fake.addWardrobeItem(999999001)
	evs := checkEvents(t, h)
	types := map[string]eventOut{}
	for _, ev := range evs {
		types[ev.Type] = ev
		if ev.Watch != "seller" {
			t.Errorf("event names watch %q", ev.Watch)
		}
	}
	for _, want := range []string{"item.price_changed", "item.reserved", "item.removed", "seller.new_item"} {
		if _, ok := types[want]; !ok {
			t.Errorf("missing %s in %v", want, evs)
		}
	}
	if pc := types["item.price_changed"]; pc.Item.ID != first || pc.Change["to"] != 1.5 {
		t.Errorf("price change %+v", pc)
	}
	if ev := types["item.removed"]; ev.Item.ID != second {
		t.Errorf("removed %+v", ev)
	}
	if ev := types["seller.new_item"]; ev.Item.ID != 999999001 {
		t.Errorf("new item %+v", ev)
	}
	// The same state again is quiet, and the history holds what was reported.
	if out := strings.TrimSpace(h.must("", "watch", "check", "--all", "--format", "jsonl").stdout); out != "" {
		t.Fatalf("a repeated check re-reported: %s", out)
	}
	history := decode[[]map[string]any](t, h.must("", "watch", "events").stdout)
	if len(history) != len(evs) {
		t.Fatalf("events history %d, want %d", len(history), len(evs))
	}
}

func TestSearchWatchEmitInitialAndList(t *testing.T) {
	h := newHarness(t)
	evs := decode[[]eventOut](t, h.must("", "watch", "add", "search", "nike", "--price-to", "30", "--name", "nike", "--emit-initial").stdout)
	if len(evs) < 2 || evs[0].Type != "item.new" || evs[0].Item.ID == 0 {
		t.Fatalf("emit-initial %v", evs)
	}
	list := decode[[]map[string]any](t, h.must("", "watch", "list").stdout)
	if len(list) != 1 || list[0]["name"] != "nike" || list[0]["kind"] != "search" {
		t.Fatalf("watch list %v", list)
	}
	if pretty := h.must("", "watch", "list", "--format", "pretty").stdout; !strings.Contains(pretty, `"nike"`) {
		t.Fatalf("watch list pretty does not describe the search:\n%s", pretty)
	}
	if r := h.run("", "watch", "add", "search", "nike", "--name", "nike"); r.code != 2 {
		t.Fatalf("duplicate name: exit %d", r.code)
	}
	h.must("", "watch", "remove", "nike", "--yes")
	if r := h.run("", "watch", "remove", "nike", "--yes"); r.code != 4 {
		t.Fatalf("removing a missing watch: exit %d", r.code)
	}
}

func TestConfigAndDoctor(t *testing.T) {
	h := newHarness(t)
	h.must("", "config", "set", "profiles.default.domain", "www.vinted.es")
	if v := h.must("", "config", "get", "profiles.default.domain").stdout; !strings.Contains(v, "www.vinted.es") {
		t.Fatalf("config get %q", v)
	}
	if r := h.run("", "config", "set", "profiles.default.location", "x"); r.code != 2 {
		t.Fatalf("unknown key: exit %d", r.code)
	}
	r := h.run("", "doctor")
	rep := decode[map[string]any](t, r.stdout)
	if r.code != 1 || rep["ok"] != false {
		t.Fatalf("doctor without a session should fail: %d %v", r.code, rep)
	}
	h.login()
	if rep := decode[map[string]any](t, h.must("", "doctor").stdout); rep["ok"] != true {
		t.Fatalf("doctor %v", rep)
	}
}

func TestSkillsAreEmbedded(t *testing.T) {
	h := newHarness(t)
	names := decode[[]string](t, h.must("", "skills", "list").stdout)
	if len(names) == 0 || names[0] != "vinted-usage" {
		t.Fatalf("skills %v", names)
	}
	if doc := h.must("", "skills", "get", "vinted-usage").stdout; !strings.Contains(doc, "vinted") {
		t.Fatal("skill body empty")
	}
	if r := h.run("", "skills", "get", "nope"); r.code != 2 {
		t.Fatalf("unknown skill: exit %d", r.code)
	}
}

func TestErrorEnvelopeJSON(t *testing.T) {
	h := newHarness(t)
	r := h.run("", "--error-format", "json", "me", "show")
	env := decode[map[string]any](t, r.stderr)
	if r.code != 3 || env["code"] != float64(3) || env["category"] != "auth" {
		t.Fatalf("envelope %d %v", r.code, env)
	}
}
