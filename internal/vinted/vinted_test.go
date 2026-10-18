package vinted

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	case key == "POST /web/api/auth/refresh":
		serve("refresh.json")
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

func loggedIn(c *Client, exp time.Time) *Session {
	s := NewSession(c, map[string]string{AccessCookie: testJWT(3000000001, exp), RefreshCookie: "refresh-1", "datadome": "dd-1"})
	c.Session = s
	return s
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

func TestParseCookieExport(t *testing.T) {
	cases := map[string]string{"cookies_netscape.txt": "www.vinted.fr", "cookies.json": "www.vinted.es", "cookies_header.txt": ""}
	for name, wantHost := range cases {
		f, err := os.Open(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		cookies, host, err := ParseCookieExport(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if host != wantHost || cookies[RefreshCookie] != "fake-refresh-token" || cookies["datadome"] != "fake-datadome" || cookies[AccessCookie] == "" {
			t.Fatalf("%s: host=%q cookies=%v", name, host, cookies)
		}
		s := NewSession(New(), cookies)
		if s.UserID() != 3000000001 || s.Expires.Year() != 2100 {
			t.Fatalf("%s: uid=%d exp=%v", name, s.UserID(), s.Expires)
		}
	}
	wrapped := `{"cookies":[{"name":"refresh_token_web","value":"r","domain":"www.vinted.it"}]}`
	if c, host, err := ParseCookieExport(strings.NewReader(wrapped)); err != nil || host != "www.vinted.it" || c[RefreshCookie] != "r" {
		t.Fatalf("wrapped json: %v %q %v", c, host, err)
	}
	// Cookie-Editor's wrapper: the site comes from url, even when the cookies
	// sit on the parent domain. All values are synthetic.
	editor := `{"url":"https://www.vinted.com","cookies":[` +
		`{"domain":".vinted.com","name":"access_token_web","value":"` + testJWT(3000000001, time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)) + `","httpOnly":false,"expirationDate":4102444800,"path":"/","secure":true},` +
		`{"domain":".vinted.com","name":"refresh_token_web","value":"synthetic-refresh","httpOnly":true,"expirationDate":4102444800,"hostOnly":false},` +
		`{"domain":"www.vinted.com","name":"datadome","value":"synthetic-dd","httpOnly":false,"session":false}]}`
	c, host, err := ParseCookieExport(strings.NewReader(editor))
	if err != nil || host != "www.vinted.com" || c[RefreshCookie] != "synthetic-refresh" || c["datadome"] != "synthetic-dd" {
		t.Fatalf("cookie-editor wrapper: %v %q %v", c, host, err)
	}
	if s := NewSession(New(), c); s.UserID() != 3000000001 {
		t.Fatalf("cookie-editor wrapper: uid %d", s.UserID())
	}
	if _, _, err := ParseCookieExport(strings.NewReader("foo=bar; baz=qux")); err == nil {
		t.Fatal("expected an error without vinted tokens")
	}
	if _, _, err := ParseCookieExport(strings.NewReader("[{bad json")); err == nil {
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
	if err != nil || len(brands) != 2 || brands[0] != (Brand{ID: 53, Title: "Nike", Slug: "nike", Items: 62698363}) {
		t.Fatalf("brands=%+v err=%v", brands, err)
	}
}

func TestLoggedInCalls(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c, time.Now().Add(time.Hour))
	ctx := context.Background()
	me, err := c.Me(ctx)
	if err != nil || me.ID != 3000000001 || me.Login != "sample_me" || me.PhoneVerified == nil || *me.PhoneVerified {
		t.Fatalf("me=%+v err=%v", me, err)
	}
	if h := f.headers["GET /api/v2/users/current"].Get("Cookie"); !strings.Contains(h, "refresh_token_web=refresh-1") || !strings.Contains(h, "datadome=dd-1") {
		t.Fatalf("cookie header lacks jar entries")
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
	loggedIn(c, time.Now().Add(time.Hour))
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
	loggedIn(c, time.Now().Add(time.Hour))
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
	m.EntityType, m.Entity.Title, m.Entity.Status = "offer_message", "", json.RawMessage("30")
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
	loggedIn(c, time.Now().Add(time.Hour))
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
	loggedIn(c, time.Now().Add(time.Hour))
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

func TestRefreshBeforeExpiryRotatesAndPersists(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(30*time.Second))
	var rotated map[string]string
	var rotatedExp time.Time
	s.OnRotate = func(cookies map[string]string, exp time.Time) { rotated, rotatedExp = cookies, exp }
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /web/api/auth/refresh") != 1 {
		t.Fatalf("refresh hits = %d", f.count("POST /web/api/auth/refresh"))
	}
	if rotated[RefreshCookie] != "fake-refresh-token-rotated" || rotated[AccessCookie] == "" || s.Expires.Year() != 2100 || !rotatedExp.Equal(s.Expires) {
		t.Fatalf("rotated=%v exp=%v expires=%v", rotated, rotatedExp, s.Expires)
	}
	if _, ok := rotated["datadome"]; ok {
		t.Fatal("unchanged cookies must not be handed to OnRotate")
	}
	if h := f.headers["POST /web/api/auth/refresh"].Get("Cookie"); !strings.Contains(h, "refresh_token_web=refresh-1") {
		t.Fatal("refresh did not send the refresh cookie")
	}
}

// A long-running process holding a refresh token another process already
// spent must not write it back when an unrelated cookie rotates.
func TestRotateHandsOnlyChangedCookies(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(time.Hour))
	f.override["GET /api/v2/users/current"] = func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "datadome", Value: "dd-2"})
		_, _ = w.Write(fixture(t, "users_current.json"))
	}
	var rotated map[string]string
	var rotatedExp time.Time
	s.OnRotate = func(cookies map[string]string, exp time.Time) { rotated, rotatedExp = cookies, exp }
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rotated) != 1 || rotated["datadome"] != "dd-2" || !rotatedExp.IsZero() {
		t.Fatalf("rotated=%v exp=%v", rotated, rotatedExp)
	}
}

// The fake home page hands out a guest access token, as Vinted does to a
// request it does not recognise as logged in; it must not replace the login.
func TestGuestTokensDoNotReplaceLogin(t *testing.T) {
	_, c := newFake(t)
	s := loggedIn(c, time.Now().Add(time.Hour))
	before := s.Cookies[AccessCookie]
	s.OnRotate = func(map[string]string, time.Time) { t.Error("guest tokens must not be persisted") }
	if _, _, err := c.csrfToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Cookies[AccessCookie] != before || s.UserID() != 3000000001 {
		t.Fatal("guest access token replaced the logged-in one")
	}
}

func TestRefreshIsSingleFlight(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c, time.Now().Add(-time.Minute))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.UnreadCount(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := f.count("POST /web/api/auth/refresh"); n != 1 {
		t.Fatalf("refresh ran %d times; a spent refresh token must never be replayed", n)
	}
}

func TestRefreshRejectedIsAuth(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c, time.Now().Add(-time.Minute))
	f.override["POST /web/api/auth/refresh"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture(t, "refresh_400.json"))
	}
	if _, err := c.Me(context.Background()); kindOf(err) != KindAuth {
		t.Fatalf("err = %v", err)
	}
}

func TestUnauthorizedRetriesOnceAfterRefresh(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c, time.Now().Add(time.Hour))
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
	if f.count("POST /web/api/auth/refresh") != 1 || calls != 2 {
		t.Fatalf("refresh=%d calls=%d", f.count("POST /web/api/auth/refresh"), calls)
	}
}

type memStore struct {
	mu      sync.Mutex
	jar     map[string]string
	saved   int
	locked  bool
	lockErr error
	saveErr error
}

func (m *memStore) Lock(ctx context.Context) (func(), error) {
	m.mu.Lock()
	m.locked = true
	return func() { m.locked = false; m.mu.Unlock() }, m.lockErr
}
func (m *memStore) Load() (map[string]string, error) { return m.jar, nil }
func (m *memStore) Save(c map[string]string, exp time.Time) error {
	if !m.locked {
		return errors.New("save outside lock")
	}
	if m.saveErr != nil {
		return m.saveErr
	}
	m.jar, m.saved = c, m.saved+1
	return nil
}

func TestStoreRefreshSavesUnderLock(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(-time.Minute))
	store := &memStore{jar: map[string]string{AccessCookie: s.Cookies[AccessCookie], RefreshCookie: "refresh-1"}}
	s.Store = store
	s.OnRotate = func(map[string]string, time.Time) { t.Error("OnRotate must not run inside a Store-guarded refresh") }
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.saved != 1 || store.jar[RefreshCookie] != "fake-refresh-token-rotated" || f.count("POST /web/api/auth/refresh") != 1 {
		t.Fatalf("saved=%d jar=%v", store.saved, store.jar)
	}
}

// Vinted has already rotated the pair when Save fails: memory must keep
// matching disk, the error must say a new login is likely needed, and the
// spent refresh token must never be sent again.
func TestStoreRefreshSaveFailureDoesNotAdopt(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(-time.Minute))
	oldAccess := s.Cookies[AccessCookie]
	store := &memStore{jar: map[string]string{AccessCookie: oldAccess, RefreshCookie: "refresh-1"}, saveErr: errors.New("disk full")}
	s.Store = store
	_, err := c.Me(context.Background())
	if kindOf(err) != KindAuth || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "vinted auth login") {
		t.Fatalf("err = %v", err)
	}
	if s.Cookies[AccessCookie] != oldAccess || s.Cookies[RefreshCookie] != "refresh-1" {
		t.Fatalf("rotated pair adopted in memory although Save failed: %v", s.Cookies)
	}
	if _, err := c.Me(context.Background()); kindOf(err) != KindAuth {
		t.Fatalf("second call err = %v", err)
	}
	if n := f.count("POST /web/api/auth/refresh"); n != 1 {
		t.Fatalf("refresh ran %d times; the spent refresh token was replayed", n)
	}
}

func TestStoreAdoptsTokenRotatedElsewhere(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(-time.Minute))
	fresh := testJWT(3000000001, time.Now().Add(time.Hour))
	s.Store = &memStore{jar: map[string]string{AccessCookie: fresh, RefreshCookie: "refresh-2", "datadome": "dd-2"}}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /web/api/auth/refresh") != 0 {
		t.Fatal("refreshed although another process already had")
	}
	if s.Cookies[RefreshCookie] != "refresh-2" || s.Cookies[AccessCookie] != fresh {
		t.Fatal("stored jar not adopted")
	}
}

func TestStoreUsesStoredRefreshToken(t *testing.T) {
	f, c := newFake(t)
	s := loggedIn(c, time.Now().Add(-time.Minute))
	s.Store = &memStore{jar: map[string]string{AccessCookie: testJWT(3000000001, time.Now().Add(-time.Minute)), RefreshCookie: "refresh-2"}}
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h := f.headers["POST /web/api/auth/refresh"].Get("Cookie"); !strings.Contains(h, "refresh_token_web=refresh-2") {
		t.Fatal("refresh replayed the in-memory (spent) token instead of the stored one")
	}
}

func TestStoreLoggedOut(t *testing.T) {
	_, c := newFake(t)
	s := loggedIn(c, time.Now().Add(-time.Minute))
	s.Store = &memStore{}
	if _, err := c.Me(context.Background()); kindOf(err) != KindAuth {
		t.Fatalf("err = %v", err)
	}
}

func TestDebugRedactsCookies(t *testing.T) {
	_, c := newFake(t)
	var buf strings.Builder
	c.Debug = &buf
	s := loggedIn(c, time.Now().Add(-time.Minute))
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Cookies {
		if strings.Contains(buf.String(), v) {
			t.Fatal("debug output leaked a cookie value")
		}
	}
	err := c.statusError("GET /x", 500, []byte("token "+s.Cookies[AccessCookie]))
	if strings.Contains(err.(*Error).Body, s.Cookies[AccessCookie]) {
		t.Fatal("error body leaked the access token")
	}
}
