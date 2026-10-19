package vinted

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Microck/galleton/cookiefile"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeVinted serves the testdata fixtures under the paths the client calls.
type fakeVinted struct {
	t        *testing.T
	mu       sync.Mutex
	hits     map[string]int
	bodies   map[string]string
	headers  map[string]http.Header
	override map[string]func(w http.ResponseWriter, r *http.Request)
}

func newFake(t *testing.T) (*fakeVinted, *Client) {
	f := &fakeVinted{t: t, hits: map[string]int{}, bodies: map[string]string{}, headers: map[string]http.Header{}, override: map[string]func(http.ResponseWriter, *http.Request){}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New()
	c.Base = srv.URL
	return f, c
}

func (f *fakeVinted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.hits[key]++
	f.bodies[key] = string(body)
	f.headers[key] = r.Header.Clone()
	h := f.override[key]
	f.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	serve := func(name string) {
		if strings.HasSuffix(name, ".json") {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write(fixture(f.t, name))
	}
	switch {
	case key == "GET /":
		http.SetCookie(w, &http.Cookie{Name: AccessCookie, Value: testJWT(0, time.Now().Add(time.Hour))})
		serve("home.html")
	case key == "GET /catalog":
		serve("catalog.html")
	case key == "GET /items/1000000001":
		serve("item.html")
	case key == "GET /items/1000000004":
		serve("item_sold.html")
	case key == "GET /api/v2/users/2000000001":
		serve("user.json")
	case key == "GET /api/v2/users/current":
		serve("users_current.json")
	case key == "GET /api/v2/wardrobe/2000000001/items":
		serve("wardrobe.json")
	case key == "GET /api/v2/users/3000000001/items/favourites":
		serve("favourites.json")
	case key == "GET /api/v2/user_feedbacks":
		serve("feedbacks.json")
	case key == "GET /api/v2/brands":
		serve("brands.json")
	case key == "GET /api/v2/item_upload/catalogs":
		serve("catalogs.json")
	case key == "GET /api/v2/conversations/stats":
		serve("stats.json")
	case key == "GET /api/v2/inbox":
		serve("inbox.json")
	case key == "GET /api/v2/conversations/6000000001":
		serve("conversation.json")
	case key == "POST /api/v2/conversations/6000000001/replies":
		serve("reply.json")
	case key == "POST /api/v2/user_favourites/toggle":
		_, _ = w.Write([]byte(`{"code":0}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>not found</body></html>"))
	}
}

func (f *fakeVinted) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func testJWT(sub int64, exp time.Time) string {
	payload, _ := json.Marshal(map[string]any{"sub": jsonString(sub), "exp": exp.Unix()})
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
}

func jsonString(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// testAccount stands in for the session engine: it attaches a fixed cookie
// jar, never rotates it, and records renewals.
type testAccount struct {
	mu     sync.Mutex
	cookie string
	renews int
	renew  func() error
}

func (a *testAccount) Send(ctx context.Context, method, rawURL string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = header.Clone()
	if req.Header.Get("Cookie") != "" {
		return 0, nil, nil, errors.New("the client sent its own cookies on an account request")
	}
	req.Header.Set("Cookie", a.cookie)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw, err
}

func (a *testAccount) Renew(context.Context) error {
	a.mu.Lock()
	a.renews++
	renew := a.renew
	a.mu.Unlock()
	if renew != nil {
		return renew()
	}
	return nil
}

func (a *testAccount) UserID() int64 { return 3000000001 }

func loggedIn(c *Client) *testAccount {
	a := &testAccount{cookie: AccessCookie + "=" + testJWT(3000000001, time.Now().Add(time.Hour)) + "; " + RefreshCookie + "=refresh-1; datadome=dd-1"}
	c.Account = a
	return a
}

func kindOf(err error) Kind {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Kind
	}
	return -1
}

func TestParseRSCResolvesRowsAndRefs(t *testing.T) {
	p := parseRSC(fixture(t, "item.html"))
	if p == nil {
		t.Fatal("no flight payload found")
	}
	if _, ok := p.rows["1"]; !ok {
		t.Fatal("import row (tag I) was not kept")
	}
	holder := p.find(func(m map[string]any) bool { return hasKeys(m, "brand_dto", "plugins") })
	if holder == nil {
		t.Fatal("item sidebar object not found")
	}
	resolved := p.resolve(holder, 0).(map[string]any)
	photos, ok := resolved["photos"].([]any)
	if !ok || len(photos) != 2 {
		t.Fatalf("photos reference not resolved through React element props: %#v", resolved["photos"])
	}
	if _, ok := resolved["plugins"].([]any); !ok {
		t.Fatalf("plugins reference not resolved: %#v", resolved["plugins"])
	}
}

func TestSplitRSCRowsHandlesTextAndHintRows(t *testing.T) {
	stream := "1:\"$Sreact.fragment\"\n:HL[\"/a.css\",\"style\"]\na:T5,ab\ncd0:{\"x\":\"$undefined\",\"y\":\"$$dollar\",\"z\":\"$1\"}\n"
	rows := splitRSCRows([]byte(stream))
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	p := &rscPage{rows: rows, cache: map[string]any{}}
	v, _ := p.row("0")
	got := p.resolve(v, 0).(map[string]any)
	if got["x"] != nil || got["y"] != "$dollar" || got["z"] != "$Sreact.fragment" {
		t.Fatalf("resolved = %#v", got)
	}
}

func TestParseRSCWithoutPayload(t *testing.T) {
	if parseRSC([]byte("<html><body>hello</body></html>")) != nil {
		t.Fatal("expected nil for a page without flight data")
	}
}

func TestSearch(t *testing.T) {
	f, c := newFake(t)
	var query string
	f.override["GET /catalog"] = func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write(fixture(t, "catalog.html"))
	}
	page, err := c.Search(context.Background(), SearchParams{
		Text: "air max", CatalogIDs: []int{1242}, BrandIDs: []int{53, 14}, StatusIDs: []int{2}, PriceFrom: 10, PriceTo: 50.5,
		Order: "newest_first", Page: 2, Extra: map[string][]string{"currency": {"EUR"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"search_text=air+max", "catalog%5B%5D=1242", "brand_ids%5B%5D=53", "brand_ids%5B%5D=14", "status_ids%5B%5D=2", "price_from=10", "price_to=50.5", "order=newest_first", "page=2", "currency=EUR"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %q", query, want)
		}
	}
	if page.Page != 2 || page.TotalPages != 3 || page.Total != 250 || len(page.Items) != 2 {
		t.Fatalf("page = %+v", page)
	}
	a := page.Items[0]
	if a.ID != 1000000001 || a.Title != "Nike Air Max 90" || a.Price != 45 || a.TotalPrice != 48.85 || a.Currency != "EUR" ||
		a.Brand != "Nike" || a.Size != "42" || a.Condition != "Very good" || a.SellerID != 2000000001 || a.Favorites != 12 ||
		a.URL != c.Base+"/items/1000000001-nike-air-max-90" || len(a.Images) != 1 {
		t.Fatalf("item = %+v", a)
	}
	b := page.Items[1]
	if b.Size != "" || b.Condition != "Good" || !b.Promoted {
		t.Fatalf("second item = %+v", b)
	}
	// Guest mode minted a session from the home page first.
	if f.count("GET /") != 1 {
		t.Fatalf("guest bootstrap hits = %d", f.count("GET /"))
	}
}

func TestSearchPerPageTrimsAndValidates(t *testing.T) {
	_, c := newFake(t)
	page, err := c.Search(context.Background(), SearchParams{Text: "x", PerPage: 1})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := c.Search(context.Background(), SearchParams{Order: "cheapest"}); kindOf(err) != KindUsage {
		t.Fatalf("bad order err = %v", err)
	}
	if _, err := c.Search(context.Background(), SearchParams{PriceFrom: 20, PriceTo: 10}); kindOf(err) != KindUsage {
		t.Fatalf("inverted prices err = %v", err)
	}
}

func TestParseEmptyCatalog(t *testing.T) {
	c := New()
	page, err := c.parseCatalogPage(fixture(t, "catalog_empty.html"))
	if err != nil || len(page.Items) != 0 || page.Total != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestItem(t *testing.T) {
	_, c := newFake(t)
	it, err := c.Item(context.Background(), 1000000001)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Nike Air Max 90" || it.Description != "Worn twice.\nOriginal box included." || it.Price != 45 || it.Currency != "EUR" ||
		it.TotalPrice != 48.85 || it.Brand != "Nike" || it.Size != "42" || it.Condition != "Very good" || it.CategoryID != 1242 ||
		it.SellerID != 2000000001 || it.SellerLogin != "sample_seller" || it.Favorites != 12 || !it.Favorited || it.Sold || len(it.Images) != 2 ||
		it.URL != "https://www.vinted.fr/items/1000000001-nike-air-max-90" {
		t.Fatalf("item = %+v", it)
	}
}

func TestItemSold(t *testing.T) {
	c := New()
	it, err := c.parseItemPage(fixture(t, "item_sold.html"), 1000000001)
	if err != nil || !it.Sold || it.Reserved {
		t.Fatalf("item=%+v err=%v", it, err)
	}
}

// Issue #2: user_info_header carries its badges as plain strings. Badges in
// any shape decode, the rest of the item still reads, and what the cli cannot
// name stays in Unparsed together with blocks it has no reader for.
func TestItemToleratesOddPluginShapes(t *testing.T) {
	c := New()
	it, err := c.parseItemPage(fixture(t, "item_badges.html"), 1000000001)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Nike Air Max 90" || it.Description == "" || it.Size != "42" || it.Seller == nil || it.Seller.Login != "sample_seller" || it.Seller.Reviews != 4 {
		t.Fatalf("item = %+v", it)
	}
	if !slices.Equal(it.Seller.Badges, []string{"ACTIVE_LISTER", "Fast shipper", "Pro"}) {
		t.Fatalf("badges = %v", it.Seller.Badges)
	}
	want := map[string]string{
		"plugins.user_info_header.badges.1":   `{"odd":true}`,
		"plugins.seller_badges_info.badges.2": `7`,
		"plugins.new_block":                   `{"note":"new block"}`,
	}
	if len(it.Unparsed) != len(want) {
		t.Fatalf("unparsed = %s", it.Unparsed)
	}
	for k, v := range want {
		if string(it.Unparsed[k]) != v {
			t.Errorf("unparsed[%s] = %s, want %s", k, it.Unparsed[k], v)
		}
	}
}

// A plugin whose data has a shape its reader does not know keeps that data
// raw; the item and the other plugins still read.
func TestItemKeepsAnUnreadablePluginRaw(t *testing.T) {
	c := New()
	page := bytes.Replace(fixture(t, "item.html"), []byte(`{\"description\":\"Worn twice.\\nOriginal box included.\",`), []byte(`{\"description\":{\"text\":\"Worn twice.\"},`), 1)
	it, err := c.parseItemPage(page, 1000000001)
	if err != nil {
		t.Fatal(err)
	}
	if it.Description != "" || it.Size != "42" || !strings.Contains(string(it.Unparsed["plugins.description"]), `"text":"Worn twice."`) {
		t.Fatalf("item = %+v unparsed = %s", it, it.Unparsed)
	}
}

func TestItemNotFound(t *testing.T) {
	_, c := newFake(t)
	_, err := c.Item(context.Background(), 42)
	if kindOf(err) != KindNotFound || !strings.Contains(err.Error(), "item 42") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRefs(t *testing.T) {
	items := map[string]int64{
		"123": 123,
		"https://www.vinted.fr/items/4567-robe-zara":  4567,
		"https://www.vinted.co.uk/items/89?ref=share": 89,
		"/items/77-slug":       77,
		"vinted.es/items/55-x": 0,
	}
	for ref, want := range items {
		got, err := ParseItemRef(ref)
		if want == 0 {
			if err == nil {
				t.Errorf("ParseItemRef(%q) = %d, want error", ref, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParseItemRef(%q) = %d, %v; want %d", ref, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "https://example.com/items/1", "https://www.vinted.fr/member/12-x", "-5",
		"https://vinted.evil.com/items/1", "https://www.vinted.com.evil.com/items/1", "https://evil.vinted.fr/items/1",
		"https://www.vinted.fr.evil.com/items/1", "https://vinted/items/1", "https://www.www.vinted.fr/items/1",
		"https://vinted.example/items/1", "https://notvinted.com/items/1", "https://www.vinted.fr@evil.com/items/1"} {
		if _, err := ParseItemRef(bad); kindOf(err) != KindUsage {
			t.Errorf("ParseItemRef(%q) err = %v, want usage", bad, err)
		}
	}
	for _, good := range []string{"https://vinted.fr/items/1", "https://WWW.Vinted.CO.UK/items/1", "https://www.vinted.com:443/items/1", "https://www.vinted.lv/items/1"} {
		if got, err := ParseItemRef(good); err != nil || got != 1 {
			t.Errorf("ParseItemRef(%q) = %d, %v; want 1", good, got, err)
		}
	}
	users := map[string]int64{"42": 42, "https://www.vinted.fr/member/1234567890-login": 1234567890, "https://www.vinted.de/member/general/7": 7}
	for ref, want := range users {
		if got, err := ParseUserRef(ref); err != nil || got != want {
			t.Errorf("ParseUserRef(%q) = %d, %v; want %d", ref, got, err, want)
		}
	}
	if _, err := ParseUserRef("https://www.vinted.fr/items/1-x"); kindOf(err) != KindUsage {
		t.Errorf("item URL accepted as user ref: %v", err)
	}
}

func TestReadCookieExport(t *testing.T) {
	cases := map[string]string{"cookies_netscape.txt": "www.vinted.fr", "cookies.json": "www.vinted.es", "cookies_header.txt": ""}
	for name, wantSite := range cases {
		f, err := os.Open(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		export, err := ReadCookieExport(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		values := map[string]string{}
		for _, c := range export.Cookies {
			values[c.Name] = c.Value
		}
		if export.Site != wantSite || values[RefreshCookie] != "fake-refresh-token" || values["datadome"] != "fake-datadome" || values[AccessCookie] == "" {
			t.Fatalf("%s: site=%q cookies=%v", name, export.Site, values)
		}
		if exp := AccessExpiry(export.Cookies); exp.Year() != 2100 {
			t.Fatalf("%s: access expiry %v", name, exp)
		}
	}
	// Cookie-Editor's wrapper: the site comes from the token cookies' domain.
	// All values are synthetic.
	editor := `{"url":"https://www.vinted.com","cookies":[` +
		`{"domain":".www.vinted.com","name":"access_token_web","value":"` + testJWT(3000000001, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) + `","httpOnly":false,"expirationDate":4102444800,"path":"/","secure":true},` +
		`{"domain":".www.vinted.com","name":"refresh_token_web","value":"synthetic-refresh","httpOnly":true,"expirationDate":4102444800,"hostOnly":false},` +
		`{"domain":".vinted.com","name":"datadome","value":"synthetic-dd","httpOnly":false,"session":false},` +
		`{"domain":".example.com","name":"other","value":"elsewhere"}]}`
	export, err := ReadCookieExport(strings.NewReader(editor))
	if err != nil || export.Site != "www.vinted.com" {
		t.Fatalf("cookie-editor wrapper: %+v %v", export, err)
	}
	kept := map[string]bool{}
	for _, c := range export.For(DefaultBase) {
		kept[c.Name] = true
	}
	if !kept[RefreshCookie] || !kept["datadome"] || kept["other"] {
		t.Fatalf("export narrowed to www.vinted.com: %v", kept)
	}
	if got := export.For("https://www.vinted.fr"); len(got) != 0 {
		t.Fatalf("www.vinted.com cookies offered to www.vinted.fr: %v", got)
	}
	// A whole-browser export with logins on two sites names no site, and
	// each site's cookies carry that site's own access token expiry.
	two := `[` +
		`{"domain":".www.vinted.fr","name":"access_token_web","value":"` + testJWT(3000000001, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) + `"},` +
		`{"domain":".www.vinted.fr","name":"refresh_token_web","value":"synthetic-refresh-fr"},` +
		`{"domain":".www.vinted.es","name":"access_token_web","value":"` + testJWT(3000000002, time.Date(2090, 1, 1, 0, 0, 0, 0, time.UTC)) + `"},` +
		`{"domain":".www.vinted.es","name":"refresh_token_web","value":"synthetic-refresh-es"}]`
	export, err = ReadCookieExport(strings.NewReader(two))
	if err != nil || export.Site != "" || !slices.Equal(export.Sites, []string{"www.vinted.es", "www.vinted.fr"}) {
		t.Fatalf("two-site export: site=%q sites=%v err=%v", export.Site, export.Sites, err)
	}
	if fr, es := AccessExpiry(export.For("https://www.vinted.fr")), AccessExpiry(export.For("https://www.vinted.es")); fr.Year() != 2100 || es.Year() != 2090 {
		t.Fatalf("expiries fr=%v es=%v", fr, es)
	}
	if _, err := ReadCookieExport(strings.NewReader("foo=bar; baz=qux")); err == nil {
		t.Fatal("expected an error without vinted tokens")
	}
	if _, err := ReadCookieExport(strings.NewReader("[{bad json")); err == nil {
		t.Fatal("expected an error for broken json")
	}
}

func TestStatusErrorClassification(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   []byte
		kind   Kind
		code   int
	}{
		{400, fixture(t, "error_115.json"), KindAuth, 115},
		{401, fixture(t, "error_100.json"), KindAuth, 100},
		{403, []byte(`{"code":106,"message":"csrf"}`), KindGeneric, 106},
		{404, []byte("<!DOCTYPE html><html></html>"), KindNotFound, 0},
		{500, nil, KindGeneric, 0},
		{422, []byte(`{"code":99,"message":"Price too low"}`), KindGeneric, 99},
		{302, nil, KindAPIChanged, 0},
	}
	for _, tc := range cases {
		err := c.statusError("GET /x", tc.status, tc.body)
		var ve *Error
		if !errors.As(err, &ve) || ve.Kind != tc.kind || ve.APICode != tc.code {
			t.Errorf("HTTP %d: got %+v, want kind %v code %d", tc.status, err, tc.kind, tc.code)
		}
	}
	err := c.statusError("GET /x", 400, fixture(t, "error_115.json"))
	if !strings.Contains(err.Error(), "verif") {
		t.Errorf("115 message should tell the user to verify: %q", err)
	}
	err = c.statusError("GET /x", 422, []byte(`{"code":99,"message":"Price too low"}`))
	if err.Error() != "Price too low" {
		t.Errorf("envelope message not surfaced: %q", err)
	}
}

func TestUsersAndCatalogs(t *testing.T) {
	_, c := newFake(t)
	ctx := context.Background()
	u, err := c.User(ctx, 2000000001)
	if err != nil || u.Login != "sample_seller" || u.Country != "France" || u.City != "Lyon" || u.Items != 2 || u.Reviews != 4 ||
		u.Followers != 7 || u.Following != 3 || u.Rating != 4.75 || !u.EmailVerified || u.PhoneVerified != nil || u.LastSeen.IsZero() {
		t.Fatalf("user=%+v err=%v", u, err)
	}
	if _, err := c.User(ctx, 99); kindOf(err) != KindNotFound {
		t.Fatalf("missing user err = %v", err)
	}
	wardrobe, err := c.UserItems(ctx, 2000000001, 1, 20)
	if err != nil || len(wardrobe.Items) != 3 || wardrobe.Total != 3 {
		t.Fatalf("wardrobe=%+v err=%v", wardrobe, err)
	}
	first, reserved, sold := wardrobe.Items[0], wardrobe.Items[1], wardrobe.Items[2]
	if first.Price != 45 || first.TotalPrice != 45.9 || first.SellerLogin != "sample_seller" || first.Images[0] != "https://images1.vinted.net/t/sample/f800/1000000001-full.webp" ||
		first.CreatedAt.Unix() != 1700000000 || !reserved.Reserved || !sold.Sold {
		t.Fatalf("items = %+v", wardrobe.Items)
	}
	reviews, err := c.Reviews(ctx, 2000000001, 1)
	if err != nil || len(reviews) != 2 || reviews[0].ByLogin != "sample_buyer" || reviews[0].Rating != 5 || reviews[0].Date.IsZero() {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
	cats, err := c.Categories(ctx)
	if err != nil || len(cats) != 2 || cats[0].Children[0].Children[0].ID != 1242 {
		t.Fatalf("cats=%+v err=%v", cats, err)
	}
	brands, err := c.Brands(ctx, "nik")
	if err != nil || len(brands) != 2 || !reflect.DeepEqual(brands[0], Brand{ID: 53, Title: "Nike", Slug: "nike", Items: 62698363}) {
		t.Fatalf("brands=%+v err=%v", brands, err)
	}
}

func TestLoggedInCalls(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	ctx := context.Background()
	me, err := c.Me(ctx)
	if err != nil || me.ID != 3000000001 || me.Login != "sample_me" || me.PhoneVerified == nil || *me.PhoneVerified {
		t.Fatalf("me=%+v err=%v", me, err)
	}
	if h := f.headers["GET /api/v2/users/current"].Get("Cookie"); !strings.Contains(h, "refresh_token_web=refresh-1") || !strings.Contains(h, "datadome=dd-1") {
		t.Fatalf("the account's cookies did not reach vinted")
	}
	favs, err := c.Favorites(ctx, 1)
	if err != nil || len(favs.Items) != 1 {
		t.Fatalf("favs=%+v err=%v", favs, err)
	}
	if err := c.ToggleFavorite(ctx, 1000000001); err != nil {
		t.Fatal(err)
	}
	if got := f.headers["POST /api/v2/user_favourites/toggle"].Get("X-CSRF-Token"); got != "00000000-0000-4000-8000-000000000000" {
		t.Fatalf("csrf header = %q", got)
	}
	if body := f.bodies["POST /api/v2/user_favourites/toggle"]; body != `{"type":"item","user_favourites":[1000000001]}` {
		t.Fatalf("toggle body = %s", body)
	}
	n, err := c.UnreadCount(ctx)
	if err != nil || n != 2 {
		t.Fatalf("unread=%d err=%v", n, err)
	}
}

func TestAuthRequired(t *testing.T) {
	_, c := newFake(t)
	ctx := context.Background()
	if _, err := c.Me(ctx); kindOf(err) != KindAuth {
		t.Fatalf("Me without session: %v", err)
	}
	if _, err := c.Favorites(ctx, 1); kindOf(err) != KindAuth {
		t.Fatalf("Favorites without session: %v", err)
	}
	if _, err := c.Inbox(ctx, 1, 10); kindOf(err) != KindAuth {
		t.Fatalf("Inbox without session: %v", err)
	}
}

func TestChat(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	ctx := context.Background()
	inbox, err := c.Inbox(ctx, 1, 20)
	if err != nil || len(inbox) != 2 || !inbox[0].Unread || inbox[0].WithUser.Login != "sample_seller" || inbox[0].LastMessage != "Is it still available?" {
		t.Fatalf("inbox=%+v err=%v", inbox, err)
	}
	conv, err := c.Conversation(ctx, 6000000001)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.Messages) != 3 || !conv.Messages[0].FromSelf || conv.Messages[1].FromSelf || conv.Messages[2].FromSelf ||
		conv.Item.ID != 1000000001 || conv.Item.Price != 45 || !conv.Unread {
		t.Fatalf("conversation = %+v", conv)
	}
	msg, err := c.SendMessage(ctx, 6000000001, "Great, buying now")
	if err != nil || msg.ID != 7000000003 || !msg.FromSelf {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	if body := f.bodies["POST /api/v2/conversations/6000000001/replies"]; !strings.Contains(body, `"body":"Great, buying now"`) {
		t.Fatalf("reply body = %s", body)
	}
	if _, err := c.SendMessage(ctx, 6000000001, "  "); kindOf(err) != KindUsage {
		t.Fatalf("empty message err = %v", err)
	}
}

func TestStartConversation(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["POST /api/v2/conversations"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"conversation":{"id":6000000001},"code":0}`))
	}
	conv, err := c.StartConversation(context.Background(), 1000000001, "Hello")
	if err != nil || conv.ID != 6000000001 || conv.WithUser.ID != 2000000001 || conv.Item.ID != 1000000001 ||
		len(conv.Messages) != 1 || !conv.Messages[0].FromSelf || conv.LastMessage != conv.Messages[0].Text {
		t.Fatalf("conv=%+v err=%v", conv, err)
	}
	var created map[string]any
	_ = json.Unmarshal([]byte(f.bodies["POST /api/v2/conversations"]), &created)
	if created["item_id"] != float64(1000000001) || created["opposite_user_id"] != float64(2000000001) {
		t.Fatalf("create body = %v", created)
	}
	if n := f.count("GET /api/v2/conversations/6000000001"); n != 0 {
		t.Fatalf("start reloaded the conversation %d times", n)
	}

	// Vinted can open the conversation and still refuse the message.
	f.override["POST /api/v2/conversations/6000000001/replies"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":0,"message":"Access denied"}`))
	}
	_, err = c.StartConversation(context.Background(), 1000000001, "Hello")
	if kindOf(err) != KindGeneric || !strings.Contains(err.Error(), "conversation 6000000001 with the seller exists, but the message was not sent: Access denied") {
		t.Fatalf("refused reply err = %v", err)
	}
}

func TestOfferMessagesAreLabelled(t *testing.T) {
	var m rawMessage
	if err := json.Unmarshal([]byte(`{"id":1,"entity_type":"offer_request_message","created_at_ts":"2026-10-17T12:05:00+02:00",
		"entity":{"offer_request_id":5,"user_id":3000000001,"title":"$60.00","status":10,"price":"60.0","currency":"USD"}}`), &m); err != nil {
		t.Fatal(err)
	}
	got, ok := normalizeMessage(m, 3000000001, 2000000001)
	if !ok || got.Text != "offer request: $60.00 (pending)" || !got.FromSelf {
		t.Fatalf("offer request = %+v", got)
	}
	// Without a title the amount comes from the price.
	m.EntityType, m.Entity.V.Title, m.Entity.V.Status = "offer_message", "", json.RawMessage("30")
	if got, ok = normalizeMessage(m, 3000000001, 2000000001); !ok || got.Text != "counter offer: 60.00 USD (rejected)" {
		t.Fatalf("counter offer = %+v", got)
	}
}

func TestEditFormKeepsStoredColours(t *testing.T) {
	for name, stored := range map[string]map[string]any{
		"color_ids": {"color_ids": []any{float64(1), float64(12)}},
		"colors":    {"colors": []any{map[string]any{"id": float64(1), "title": "Black"}, map[string]any{"id": float64(12)}}},
		"color1_id": {"color1_id": float64(1), "color2_id": float64(12)},
	} {
		got, _ := json.Marshal(editForm(stored)["color_ids"])
		if string(got) != "[1,12]" {
			t.Errorf("%s: color_ids = %s", name, got)
		}
	}
	if got, _ := json.Marshal(editForm(map[string]any{"color1_id": nil})["color_ids"]); string(got) != "[]" {
		t.Errorf("no colour: color_ids = %s", got)
	}
}

func TestEditItemReportsSubmittedValues(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["GET /api/v2/item_upload/items/5"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"item":{"id":5,"title":"Old","price":{"amount":"10.0","currency_code":"USD"},"is_hidden":true,"color1_id":1}}`))
	}
	// Vinted answers an edit with little more than the id.
	f.override["PUT /api/v2/item_upload/items/5"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"item":{"id":5},"code":0}`))
	}
	title := "New"
	up, err := c.EditItem(context.Background(), 5, ItemEdit{Title: &title})
	if err != nil || up.Title != "New" || up.Price != 10 || up.Currency != "USD" || !up.Hidden {
		t.Fatalf("edited = %+v err=%v", up, err)
	}
	var sent struct {
		Item map[string]any `json:"item"`
	}
	_ = json.Unmarshal([]byte(f.bodies["PUT /api/v2/item_upload/items/5"]), &sent)
	if got, _ := json.Marshal(sent.Item["color_ids"]); string(got) != "[1]" {
		t.Fatalf("sent color_ids = %s", got)
	}
}

func TestInboxVerificationRequired(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["GET /api/v2/inbox"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture(t, "error_115.json"))
	}
	_, err := c.Inbox(context.Background(), 1, 5)
	if kindOf(err) != KindAuth || !strings.Contains(err.Error(), "verif") {
		t.Fatalf("err = %v", err)
	}
	var ve *Error
	if !errors.As(err, &ve) || len(ve.Suggest) != 1 || ve.Suggest[0] != "open "+c.Base+"/users/verification" {
		t.Fatalf("suggest = %+v", ve)
	}
}

// The client never renews by itself: a guest page handing out tokens goes
// to the Account (the engine keeps only renewals' tokens), and nothing in
// this process calls the refresh endpoint.
func TestAccountRequestsNeverCallRefresh(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	if _, _, err := c.csrfToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST " + RefreshPath); n != 0 {
		t.Fatalf("the client called the refresh endpoint %d times", n)
	}
}

func TestUnauthorizedRetriesOnceAfterRenewal(t *testing.T) {
	f, c := newFake(t)
	acct := loggedIn(c)
	calls := 0
	f.override["GET /api/v2/conversations/stats"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(fixture(t, "error_100.json"))
			return
		}
		_, _ = w.Write(fixture(t, "stats.json"))
	}
	if n, err := c.UnreadCount(context.Background()); err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if acct.renews != 1 || calls != 2 {
		t.Fatalf("renews=%d calls=%d", acct.renews, calls)
	}
}

func TestRenewalFailureEndsTheCall(t *testing.T) {
	f, c := newFake(t)
	acct := loggedIn(c)
	acct.renew = func() error { return &Error{Kind: KindAuth, Msg: "run `vinted auth login`"} }
	f.override["GET /api/v2/users/current"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write(fixture(t, "error_100.json"))
	}
	if _, err := c.Me(context.Background()); kindOf(err) != KindAuth {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("GET /api/v2/users/current"); n != 1 {
		t.Fatalf("retried %d times after a failed renewal", n)
	}
}

// An item page redirects to its canonical slug; the account request follows
// it on the same host, never to another one.
func TestAccountFollowsSameHostRedirectsOnly(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["GET /items/1000000001"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/items/1000000001-slug", http.StatusMovedPermanently)
	}
	f.override["GET /items/1000000001-slug"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "item.html"))
	}
	if _, err := c.Item(context.Background(), 1000000001); err != nil {
		t.Fatal(err)
	}
	if f.count("GET /items/1000000001-slug") != 1 {
		t.Fatal("redirect not followed")
	}
	f.override["GET /items/1000000004"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.invalid/items/1000000004", http.StatusFound)
	}
	if _, err := c.Item(context.Background(), 1000000004); err == nil {
		t.Fatal("a cross-host redirect was followed or ignored")
	}
}

func TestDebugRedactsCookies(t *testing.T) {
	_, c := newFake(t)
	var buf strings.Builder
	c.Debug = &buf
	if _, err := c.Search(context.Background(), SearchParams{Text: "nike"}); err != nil {
		t.Fatal(err)
	}
	g, err := c.guestJar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tok := g.cookies[AccessCookie]
	if tok == "" || strings.Contains(buf.String(), tok) {
		t.Fatal("debug output leaked the guest token")
	}
	err = c.statusError("GET /x", 500, []byte("token "+tok))
	if strings.Contains(err.(*Error).Body, tok) {
		t.Fatal("error body leaked the access token")
	}
}

func TestAdapterCoversEverySiteWithItsGateway(t *testing.T) {
	raw, err := Adapter(DefaultBase, "VINTED_GALLETON_PROXY", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers []adapterProvider `json:"providers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Providers) != len(siteLanguages)+1 {
		t.Fatalf("%d providers for %d sites", len(doc.Providers), len(siteLanguages)+1)
	}
	com := doc.Providers[0]
	if com.Name != "vinted" || com.CookieDomain != ".www.vinted.com" || com.MinIntervalMS != 2000 || com.ProxyEnv != "VINTED_GALLETON_PROXY" ||
		strings.Join(com.Origins, " ") != "https://www.vinted.com https://api.www.vinted.com" || com.MaxRequestBodyBytes < MaxPhotoBytes {
		t.Fatalf("www.vinted.com provider %+v", com)
	}
	if n := len(AllOrigins(DefaultBase)); n != 2*len(doc.Providers) {
		t.Fatalf("AllOrigins has %d origins for %d providers", n, len(doc.Providers))
	}
}

func TestRefreshLifetimeReadsTheTokenClaimsAndCookieExpiry(t *testing.T) {
	enc := base64.RawURLEncoding
	tok := enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString([]byte(`{"iat":1000,"exp":605800}`)) + ".sig"
	issued, expires := RefreshLifetime([]cookiefile.Cookie{{Name: RefreshCookie, Value: tok}})
	if issued.Unix() != 1000 || expires.Unix() != 605800 {
		t.Fatalf("jwt claims: %v %v", issued, expires)
	}
	cookie := time.Unix(700000, 0)
	issued, expires = RefreshLifetime([]cookiefile.Cookie{{Name: RefreshCookie, Value: tok, Expires: &cookie}})
	if issued.Unix() != 1000 || !expires.Equal(cookie) {
		t.Fatalf("cookie expiry wins: %v %v", issued, expires)
	}
	issued, expires = RefreshLifetime([]cookiefile.Cookie{{Name: RefreshCookie, Value: "opaque"}})
	if !issued.IsZero() || !expires.IsZero() {
		t.Fatalf("opaque token: %v %v", issued, expires)
	}
}
