package cli_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeVinted is a small Vinted: the JSON endpoints the CLI calls, the
// server-rendered catalog and item pages, and the cookie/token lifecycle. Its
// bodies are the internal/vinted testdata fixtures served verbatim, so these
// tests and the API package's own tests read the same shapes. Wardrobe items
// are held decoded so watch tests can change prices and flags between checks.
type fakeVinted struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	requests  []fakeRequest
	wardrobe  map[string]any
	revoked   bool // the refresh token no longer works
	refreshes int
	refresh   string // the one refresh token the server accepts next
	toggles   []string
	replies   []string
	access    string // the logged-in access token currently accepted
	// block, when set, answers every request whose path starts with
	// blockPath the way Datadome does when it blocks the client.
	block     *fakeBlock
	blockPath string
	// itemPage, when set, replaces the item.html fixture, so item watch
	// tests can change the listing between checks.
	itemPage []byte
}

type fakeBlock struct {
	status     int
	body       []byte
	retryAfter string
}

func (f *fakeVinted) setBlock(path string, b *fakeBlock) {
	f.mu.Lock()
	f.block, f.blockPath = b, path
	f.mu.Unlock()
}

func (f *fakeVinted) refreshCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes
}

// tokens are the access and refresh tokens the server accepts right now.
func (f *fakeVinted) tokens() (access, refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access, f.refresh
}

func (f *fakeVinted) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type fakeRequest struct {
	Method string
	Path   string
	Query  url.Values
}

const (
	fixtureDir = "../vinted/testdata"
	// The ids, login and token below are the ones in the fixtures.
	fakeUserID    = int64(3000000001)
	fixtureLogin  = "sample_me"
	fixtureItemID = int64(1000000001)
	fakeCSRF      = "00000000-0000-4000-8000-000000000000"
	fakeRefresh   = "refresh-token-value-0123456789"
	fakeGuestUser = int64(0)
	// wardrobePriceField is the raw /api/v2 field watch tests rewrite.
	wardrobePriceField = "price"
)

func wardrobePrice(v float64) any {
	return map[string]any{"amount": strconv.FormatFloat(v, 'f', -1, 64), "currency_code": "EUR"}
}

var tokenSerial atomic.Int64

// jwt builds an unsigned token with the claims the client reads. A serial
// keeps two tokens minted in the same second distinct, as real ones are.
func jwt(sub int64, exp time.Time) string {
	enc := base64.RawURLEncoding
	claims := map[string]any{"exp": exp.Unix(), "jti": tokenSerial.Add(1)}
	if sub != 0 {
		claims["sub"] = fmt.Sprint(sub)
	}
	payload, _ := json.Marshal(claims)
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return raw
}

func newFakeVinted(t *testing.T) *fakeVinted {
	f := &fakeVinted{t: t, refresh: fakeRefresh}
	if err := json.Unmarshal(fixture(t, "wardrobe.json"), &f.wardrobe); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// cookieFile writes a Netscape export for the fake's host holding an expired
// access token and a working refresh token, so the session engine renews
// right after the import. The export also carries another site's cookie,
// as a whole-browser export does; it must not be imported.
func (f *fakeVinted) cookieFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cookies.txt")
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	f.mu.Lock()
	refresh := f.refresh
	f.mu.Unlock()
	body := "# Netscape HTTP Cookie File\n" +
		fmt.Sprintf("127.0.0.1\tFALSE\t/\tFALSE\t%d\taccess_token_web\t%s\n", exp, jwt(fakeUserID, time.Now().Add(-time.Hour))) +
		fmt.Sprintf("#HttpOnly_127.0.0.1\tFALSE\t/\tFALSE\t%d\trefresh_token_web\t%s\n", exp, refresh) +
		fmt.Sprintf("127.0.0.1\tFALSE\t/\tFALSE\t%d\tdatadome\tdatadome-cookie-value\n", exp) +
		fmt.Sprintf(".example.com\tTRUE\t/\tTRUE\t%d\tunrelated\tunrelated-cookie-value\n", exp)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *fakeVinted) requestsTo(path string) []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeRequest
	for _, r := range f.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// setWardrobeItem changes one raw field of the wardrobe item with that id.
func (f *fakeVinted) setWardrobeItem(id int64, field string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, raw := range f.wardrobe["items"].([]any) {
		it := raw.(map[string]any)
		if int64(it["id"].(float64)) == id {
			it[field] = value
			return
		}
	}
	f.t.Fatalf("no wardrobe item %d", id)
}

// removeWardrobeItem drops one item, the way a sold-and-hidden listing goes.
func (f *fakeVinted) removeWardrobeItem(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.wardrobe["items"].([]any)
	kept := items[:0]
	for _, raw := range items {
		if int64(raw.(map[string]any)["id"].(float64)) != id {
			kept = append(kept, raw)
		}
	}
	f.wardrobe["items"] = kept
}

// addWardrobeItem appends a copy of the first item under a new id.
func (f *fakeVinted) addWardrobeItem(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.wardrobe["items"].([]any)
	clone := map[string]any{}
	for k, v := range items[0].(map[string]any) {
		clone[k] = v
	}
	clone["id"] = float64(id)
	f.wardrobe["items"] = append(items, clone)
}

func (f *fakeVinted) authorized(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access == "" {
		return false
	}
	if c, err := r.Cookie("access_token_web"); err == nil && c.Value == f.access {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+f.access
}

func (f *fakeVinted) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query()})
	block, blockPath := f.block, f.blockPath
	f.mu.Unlock()
	if block != nil && strings.HasPrefix(r.URL.Path, blockPath) {
		if block.retryAfter != "" {
			w.Header().Set("Retry-After", block.retryAfter)
		}
		w.WriteHeader(block.status)
		_, _ = w.Write(block.body)
		return
	}

	path := r.URL.Path
	jsonBody := func(name string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(f.t, name))
	}
	html := func(name string) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(fixture(f.t, name))
	}
	needAuth := func() bool {
		if f.authorized(r) {
			return true
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":100,"message":"Invalid authentication token","message_code":"invalid_authentication_token"}`))
		return false
	}

	switch {
	case r.Method == http.MethodGet && path == "/":
		if _, err := r.Cookie("access_token_web"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "access_token_web", Value: jwt(fakeGuestUser, time.Now().Add(time.Hour)), Path: "/"})
		}
		html("home.html")
	case r.Method == http.MethodPost && path == "/web/api/auth/refresh":
		// Like Vinted: the refresh token is single use, and both tokens
		// rotate in Set-Cookie (the refresh one HttpOnly).
		c, err := r.Cookie("refresh_token_web")
		f.mu.Lock()
		if err != nil || c.Value != f.refresh || f.revoked {
			f.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":100,"message":"invalid refresh token"}`))
			return
		}
		tok := jwt(fakeUserID, time.Now().Add(time.Hour))
		f.refreshes++
		f.access = tok
		f.refresh = fmt.Sprintf("%s-%d", fakeRefresh, f.refreshes)
		refresh := f.refresh
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "access_token_web", Value: tok, Path: "/", MaxAge: 3600})
		http.SetCookie(w, &http.Cookie{Name: "refresh_token_web", Value: refresh, Path: "/", HttpOnly: true, MaxAge: 7 * 86400})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "refresh_token": refresh, "expires_in": 3600, "created_at": time.Now().Unix()})
	case path == "/catalog":
		html("catalog.html")
	case strings.HasPrefix(path, "/items/"):
		if !strings.HasPrefix(strings.TrimPrefix(path, "/items/"), fmt.Sprint(fixtureItemID)) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!doctype html><html>not found</html>"))
			return
		}
		f.mu.Lock()
		page := f.itemPage
		f.mu.Unlock()
		if page != nil {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(page)
			return
		}
		html("item.html")
	case path == "/api/v2/users/current":
		if needAuth() {
			jsonBody("users_current.json")
		}
	case path == fmt.Sprintf("/api/v2/users/%d/items/favourites", fakeUserID):
		if needAuth() {
			jsonBody("favourites.json")
		}
	case strings.HasPrefix(path, "/api/v2/users/"):
		jsonBody("user.json")
	case strings.HasPrefix(path, "/api/v2/wardrobe/"):
		f.mu.Lock()
		raw, _ := json.Marshal(f.wardrobe)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	case path == "/api/v2/user_feedbacks":
		jsonBody("feedbacks.json")
	case path == "/api/v2/brands":
		jsonBody("brands.json")
	case path == "/api/v2/item_upload/catalogs":
		jsonBody("catalogs.json")
	case path == "/api/v2/inbox":
		if needAuth() {
			jsonBody("inbox.json")
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/replies"):
		if needAuth() {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			f.mu.Lock()
			f.replies = append(f.replies, string(raw))
			f.mu.Unlock()
			jsonBody("reply.json")
		}
	case strings.HasPrefix(path, "/api/v2/conversations/"):
		if needAuth() {
			jsonBody("conversation.json")
		}
	case r.Method == http.MethodPost && path == "/api/v2/user_favourites/toggle":
		if !needAuth() {
			return
		}
		if r.Header.Get("X-CSRF-Token") != fakeCSRF {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":106,"message":"csrf"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		f.mu.Lock()
		f.toggles = append(f.toggles, string(raw))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"code":0}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"message":"Not found"}`))
	}
}

func fixtureItemRef() string { return fmt.Sprint(fixtureItemID) }

// jsonID renders a JSON number id as the CLI argument form.
func jsonID(v any) string {
	f, _ := v.(float64)
	return strconv.FormatInt(int64(f), 10)
}

// wardrobeIDs returns the ids of the first two wardrobe fixture items.
func wardrobeIDs(t *testing.T, h *harness) (int64, int64) {
	t.Helper()
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	items := h.fake.wardrobe["items"].([]any)
	if len(items) < 2 {
		t.Fatal("wardrobe fixture needs two items")
	}
	id := func(i int) int64 { return int64(items[i].(map[string]any)["id"].(float64)) }
	return id(0), id(1)
}

// checkEvents runs every watch once and returns the events it printed.
func checkEvents(t *testing.T, h *harness) []eventOut {
	t.Helper()
	var out []eventOut
	for _, line := range strings.Split(strings.TrimSpace(h.must("", "watch", "check", "--all", "--format", "jsonl").stdout), "\n") {
		if line != "" {
			out = append(out, decode[eventOut](t, line))
		}
	}
	return out
}
