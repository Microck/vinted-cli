package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Microck/vinted-cli/internal/cli"
)

// catalogFake fronts fakeVinted with the API gateway's filter service (the
// fixtures recorded for catalog 1845) and Vinted's image host. Everything
// else falls through to the shared fake.
type catalogFake struct {
	h   *harness
	srv *httptest.Server

	mu    sync.Mutex
	calls []fakeRequest
}

var fakePhoto = []byte("RIFF\x10\x00\x00\x00WEBPVP8 test photo")

func newCatalogHarness(t *testing.T) (*harness, *catalogFake) {
	t.Helper()
	h := newHarness(t)
	cf := &catalogFake{h: h}
	cf.srv = httptest.NewServer(http.HandlerFunc(cf.serve))
	t.Cleanup(cf.srv.Close)
	t.Setenv("VINTED_BASE_URL", cf.srv.URL)
	return h, cf
}

func (cf *catalogFake) serve(w http.ResponseWriter, r *http.Request) {
	cf.mu.Lock()
	cf.calls = append(cf.calls, fakeRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query()})
	cf.mu.Unlock()
	t := cf.h.t
	switch path := r.URL.Path; {
	case path == "/web/gateway/svc-filters/filters":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "filters.json"))
	case path == "/web/gateway/svc-filters/filters/facets":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "filters_facets_"+r.URL.Query().Get("filter_code")+".json"))
	case strings.HasPrefix(path, "/t/"):
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(fakePhoto)
	default:
		cf.h.fake.serve(w, r)
	}
}

func (cf *catalogFake) callsTo(path string) []fakeRequest {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	var out []fakeRequest
	for _, c := range cf.calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func TestSearchFilterFlags(t *testing.T) {
	h := newHarness(t)
	h.must("", "search", "jeans", "--catalog", "1845", "--pattern", "21", "--color", "9", "--condition", "new_without_tags",
		"--material", "303", "--video-game-rating", "170", "--sim-lock", "3", "--attr", "frame_size=1,2", "--attr", "book_genre_ids=5",
		"--filter", "popular=true", "--filter", "x[]=a,b", "--filter", "x[]=c", "--currency", "usd", "--price-to", "20")
	reqs := h.fake.requestsTo("/catalog")
	if len(reqs) != 1 {
		t.Fatalf("catalog requests = %d", len(reqs))
	}
	q := reqs[0].Query
	for key, want := range map[string][]string{
		"catalog[]": {"1845"}, "patterns_ids[]": {"21"}, "color_ids[]": {"9"}, "status_ids[]": {"1"}, "material_ids[]": {"303"},
		"video_game_rating_ids[]": {"170"}, "sim_lock_ids[]": {"3"}, "frame_size_ids[]": {"1", "2"}, "book_genre_ids[]": {"5"},
		"popular": {"true"}, "x[]": {"a,b", "c"}, "currency": {"USD"}, "price_to": {"20"}, "search_text": {"jeans"},
	} {
		if !reflect.DeepEqual(q[key], want) {
			t.Errorf("%s = %v, want %v (query %v)", key, q[key], want, q)
		}
	}
	for _, args := range [][]string{
		{"--attr", "size=4"},      // has its own flag
		{"--attr", "patterns=21"}, // likewise (--pattern)
		{"--attr", "sim_lock=1"},
		{"--attr", "frame_size"},
		{"--attr", "frame_size=x"},
		{"--filter", "novalue"},
		{"--pattern", "-1"},
		{"--currency", "dollars"},
		{"--condition", "mint"},
	} {
		if r := h.run("", append([]string{"search", "x"}, args...)...); r.code != 2 {
			t.Errorf("%v: exit %d, stderr %s", args, r.code, r.stderr)
		}
	}
	if r := h.run("", "search", "x", "--attr", "size=4"); !strings.Contains(r.stderr, "--size") {
		t.Errorf("--attr size hint: %s", r.stderr)
	}
}

func TestSearchFiltersCommand(t *testing.T) {
	h, cf := newCatalogHarness(t)
	type option struct {
		ID      int64    `json:"id"`
		Title   string   `json:"title"`
		Type    string   `json:"type"`
		Options []option `json:"options"`
	}
	type filter struct {
		Code    string   `json:"code"`
		Multi   bool     `json:"multi"`
		Params  []string `json:"params"`
		Options []option `json:"options"`
	}
	out := decode[[]filter](t, h.must("", "search", "filters", "--catalog", "1845").stdout)
	var codes []string
	for _, f := range out {
		codes = append(codes, f.Code)
	}
	if strings.Join(codes, ",") != "size,brand,status,color,patterns,price,material" {
		t.Fatalf("codes = %v", codes)
	}
	if out[2].Options[0].ID != 6 || out[2].Options[0].Title != "New with tags" || out[0].Options[0].Type != "group" || out[0].Options[0].Options[0].ID != 1226 {
		t.Fatalf("options = %+v / %+v", out[2].Options[0], out[0].Options[0])
	}
	if q := cf.callsTo("/web/gateway/svc-filters/filters")[0].Query; q.Get("attribute_ids[catalog]") != "1845" {
		t.Fatalf("filters query = %v", q)
	}
	if n := len(cf.callsTo("/web/gateway/svc-filters/filters/facets")); n != 6 {
		t.Fatalf("facets requests = %d", n)
	}

	pretty := h.must("", "search", "filters", "--catalog", "1845", "--code", "status", "--code", "patterns", "--format", "pretty").stdout
	for _, want := range []string{"CODE", "patterns", "--pattern", "--condition", "price_from price_to currency", "New with tags", "Animal print"} {
		if !strings.Contains(pretty, want) {
			t.Errorf("pretty lacks %q:\n%s", want, pretty)
		}
	}
	if strings.Contains(pretty, "XXXS") {
		t.Error("--code loaded sizes too")
	}
	if n := len(cf.callsTo("/web/gateway/svc-filters/filters/facets")); n != 8 {
		t.Fatalf("facets requests after --code = %d", n)
	}
	if r := h.run("", "search", "filters", "--code", "nope"); r.code != 2 {
		t.Fatalf("unknown code: exit %d %s", r.code, r.stderr)
	}
}

// search --format jsonl prints the page document (items plus paging) on one
// line, like every other non-list result; per-item lines come from jq.
func TestSearchJSONLIsOnePageDocument(t *testing.T) {
	h := newHarness(t)
	out := h.must("", "search", "nike", "--format", "jsonl").stdout
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("jsonl lines = %d:\n%s", len(lines), out)
	}
	page := decode[struct {
		Items []map[string]any `json:"items"`
		Page  int              `json:"page"`
	}](t, lines[0])
	if len(page.Items) != 2 || page.Page != 2 {
		t.Fatalf("page = %+v", page)
	}
}

func TestItemShowDetails(t *testing.T) {
	h := newHarness(t)
	pretty := h.must("", "item", "show", fixtureItemRef(), "--format", "pretty").stdout
	for _, want := range []string{
		"45.00 EUR (48.85 EUR with buyer protection of 3.85 EUR)", "from 3.49 EUR, depending on the option picked at checkout (delivery: home)",
		"Black, White", "material", "Leather", "Women > Shoes > Trainers (1242)", "2 hours ago", "4.8/5 (4 reviews)", "Last seen 2 hours ago",
		"photo 1", "(original 1200x1600)", "Original box included.",
	} {
		if !strings.Contains(pretty, want) {
			t.Errorf("pretty lacks %q:\n%s", want, pretty)
		}
	}
	if n := len(h.fake.requestsTo("/api/v2/users/2000000001")); n != 0 {
		t.Fatalf("profile loaded without --seller: %d", n)
	}
	it := decode[struct {
		Seller struct {
			Login   string `json:"login"`
			Profile *struct {
				City    string `json:"city"`
				Country string `json:"country"`
			} `json:"profile"`
		} `json:"seller"`
		Photos []struct {
			Width int `json:"original_width"`
		} `json:"photos"`
	}](t, h.must("", "item", "show", fixtureItemRef(), "--seller").stdout)
	if it.Seller.Login != "sample_seller" || it.Seller.Profile == nil || it.Seller.Profile.City != "Lyon" || len(it.Photos) != 2 || it.Photos[0].Width != 1200 {
		t.Fatalf("item = %+v", it)
	}
	if n := len(h.fake.requestsTo("/api/v2/users/2000000001")); n != 1 {
		t.Fatalf("--seller profile requests = %d", n)
	}
}

func TestItemImagesDownload(t *testing.T) {
	h, cf := newCatalogHarness(t)
	// The fixture's photos live on images1.vinted.net; serve them locally.
	h.fake.mu.Lock()
	h.fake.itemPage = bytes.ReplaceAll(fixture(t, "item.html"), []byte("https://images1.vinted.net"), []byte(cf.srv.URL))
	h.fake.mu.Unlock()

	urls := decode[[]string](t, h.must("", "item", "images", fixtureItemRef()).stdout)
	if len(urls) != 2 || len(cf.callsTo("/t/sample/f800/p1.webp")) != 0 {
		t.Fatalf("urls = %v", urls)
	}
	dir := filepath.Join(t.TempDir(), "photos")
	type saved struct {
		URL         string `json:"url"`
		File        string `json:"file"`
		Bytes       int    `json:"bytes"`
		ContentType string `json:"content_type"`
	}
	one := decode[[]saved](t, h.must("", "item", "images", fixtureItemRef(), "--download", dir, "--limit", "1").stdout)
	want := filepath.Join(dir, fmt.Sprintf("%d-1.webp", fixtureItemID))
	if len(one) != 1 || one[0].File != want || one[0].Bytes != len(fakePhoto) || one[0].ContentType != "image/webp" || one[0].URL != urls[0] {
		t.Fatalf("saved = %+v", one)
	}
	if got, err := os.ReadFile(want); err != nil || !bytes.Equal(got, fakePhoto) {
		t.Fatalf("file: %q %v", got, err)
	}
	if n := len(cf.callsTo("/t/sample/f800/p1.webp")) + len(cf.callsTo("/t/sample/f800/p2.webp")); n != 1 {
		t.Fatalf("photo requests with --limit 1 = %d", n)
	}
	all := decode[[]saved](t, h.must("", "item", "images", fixtureItemRef(), "--download", dir).stdout)
	if len(all) != 2 || all[1].File != filepath.Join(dir, fmt.Sprintf("%d-2.webp", fixtureItemID)) {
		t.Fatalf("saved = %+v", all)
	}
	if r := h.run("", "item", "images", fixtureItemRef(), "--limit", "1"); r.code != 2 {
		t.Fatalf("--limit without --download: exit %d", r.code)
	}
}

func TestItemOpenValidatesWithoutRequests(t *testing.T) {
	h := newHarness(t)
	var opened []string
	t.Cleanup(cli.StubOpenBrowser(&opened))
	before := h.fake.requestCount()
	h.must("", "item", "open", fixtureItemRef())
	h.must("", "item", "open", "https://www.vinted.fr/items/42-blue-jeans?ref=x")
	base, _ := url.Parse(h.fake.srv.URL)
	if want := []string{base.String() + "/items/1000000001", "https://www.vinted.fr/items/42-blue-jeans?ref=x"}; !reflect.DeepEqual(opened, want) {
		t.Fatalf("opened = %v, want %v", opened, want)
	}
	for _, bad := range []string{"file:///etc/passwd", "ftp://www.vinted.com/items/1", "https://evil.example/items/1", "javascript:alert(1)", "nope",
		"https://vinted.evil.com/items/1", "https://www.vinted.com.evil.com/items/1", "https://www.vinted.fr@evil.com/items/1"} {
		if r := h.run("", "item", "open", bad); r.code != 2 {
			t.Errorf("%s: exit %d", bad, r.code)
		}
	}
	if len(opened) != 2 || h.fake.requestCount() != before {
		t.Fatalf("opened %v, requests %d -> %d", opened, before, h.fake.requestCount())
	}
}
