package vinted

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// serveFilters answers the gateway's filter service from the fixtures
// recorded on vinted.com for catalog 1845 and records each query.
func serveFilters(t *testing.T, f *fakeVinted) *[]url.Values {
	var mu sync.Mutex
	var queries []url.Values
	record := func(r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
	}
	f.override["GET /web/gateway/svc-filters/filters"] = func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Header.Get("Locale") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "filters.json"))
	}
	f.override["GET /web/gateway/svc-filters/filters/facets"] = func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "filters_facets_"+r.URL.Query().Get("filter_code")+".json"))
	}
	return &queries
}

func TestFilters(t *testing.T) {
	f, c := newFake(t)
	queries := serveFilters(t, f)
	filters, err := c.Filters(context.Background(), SearchParams{Text: "levis", CatalogIDs: []int{1845}, SizeIDs: []int{4, 5}, PriceTo: 30})
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	byCode := map[string]Filter{}
	for _, fl := range filters {
		codes = append(codes, fl.Code)
		byCode[fl.Code] = fl
	}
	if want := []string{"size", "brand", "status", "color", "patterns", "price", "material"}; !reflect.DeepEqual(codes, want) {
		t.Fatalf("codes = %v, want %v", codes, want)
	}
	// One list request, then one facets request per lazy filter but price.
	if f.count("GET /web/gateway/svc-filters/filters") != 1 || f.count("GET /web/gateway/svc-filters/filters/facets") != 6 {
		t.Fatalf("hits: list %d facets %d", f.count("GET /web/gateway/svc-filters/filters"), f.count("GET /web/gateway/svc-filters/filters/facets"))
	}
	q := (*queries)[0]
	if q.Get("attribute_ids[catalog]") != "1845" || q.Get("attribute_ids[size]") != "4,5" || q.Get("search_text") != "levis" || q.Get("price_to") != "30" {
		t.Fatalf("list query = %v", q)
	}
	if fq := (*queries)[1]; fq.Get("filter_code") != "size" || fq.Get("attribute_ids[catalog]") != "1845" {
		t.Fatalf("facets query = %v", fq)
	}

	status := byCode["status"]
	if !status.Multi || status.Type != "list" || !reflect.DeepEqual(status.Params, []string{"status_ids[]"}) {
		t.Fatalf("status = %+v", status)
	}
	var statuses []string
	for _, o := range status.Options {
		statuses = append(statuses, o.Title)
		if o.ID == 0 || !o.Selectable() || o.Count == 0 {
			t.Fatalf("status option %+v", o)
		}
	}
	if strings.Join(statuses, "|") != "New with tags|New without tags|Very good|Good|Satisfactory" || status.Options[0].ID != 6 || status.Options[1].ID != 1 {
		t.Fatalf("statuses = %v %+v", statuses, status.Options[:2])
	}
	if status.Options[0].Description == "" {
		t.Fatal("status descriptions dropped")
	}

	// Sizes come grouped under a heading whose string id is not a filter id.
	size := byCode["size"]
	if len(size.Options) == 0 || size.Options[0].Selectable() || size.Options[0].ID != 0 || size.Options[0].Type != "group" {
		t.Fatalf("size heading = %+v", size.Options)
	}
	if s := size.Options[0].Options; len(s) == 0 || !s[0].Selectable() || s[0].ID != 1226 || s[0].Title != "XXXS / US 00" {
		t.Fatalf("sizes = %+v", s)
	}
	if price := byCode["price"]; price.Multi || price.Type != "hybrid_price" || len(price.Options) != 0 || !reflect.DeepEqual(price.Params, []string{"price_from", "price_to", "currency"}) {
		t.Fatalf("price = %+v", price)
	}
	if col := byCode["color"]; col.Options[0].ID != 1 || col.Options[0].Title != "Black" || !strings.HasSuffix(col.Options[0].Image, "black.png") {
		t.Fatalf("color = %+v", col.Options[0])
	}
	if pat := byCode["patterns"]; pat.Params[0] != "patterns_ids[]" || len(pat.Options) == 0 {
		t.Fatalf("patterns = %+v", pat)
	}
}

func TestFiltersCodesLimitOptions(t *testing.T) {
	f, c := newFake(t)
	serveFilters(t, f)
	filters, err := c.Filters(context.Background(), SearchParams{CatalogIDs: []int{1845}}, "status")
	if err != nil {
		t.Fatal(err)
	}
	if f.count("GET /web/gateway/svc-filters/filters/facets") != 1 {
		t.Fatalf("facets hits = %d", f.count("GET /web/gateway/svc-filters/filters/facets"))
	}
	for _, fl := range filters {
		if (fl.Code == "status") != (len(fl.Options) > 0) {
			t.Fatalf("%s options = %d", fl.Code, len(fl.Options))
		}
	}
	// An unknown code is caught before any option is loaded.
	if _, err := c.Filters(context.Background(), SearchParams{}, "nope"); kindOf(err) != KindUsage || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	if f.count("GET /web/gateway/svc-filters/filters/facets") != 1 {
		t.Fatal("options loaded for a bad code")
	}
}

func TestSearchAttributesAndCurrency(t *testing.T) {
	q, err := SearchParams{
		CatalogIDs: []int{3026}, MaterialIDs: []int{303}, Currency: "USD", PriceFrom: 5,
		Attributes: map[string][]int{"video_game_rating": {170, 171}, "patterns": {21}},
	}.query()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(q["video_game_rating_ids[]"], []string{"170", "171"}) || q.Get("patterns_ids[]") != "21" ||
		q.Get("material_ids[]") != "303" || q.Get("currency") != "USD" || q.Get("price_from") != "5" {
		t.Fatalf("query = %v", q)
	}
	for name, p := range map[string]SearchParams{
		"bad code":      {Attributes: map[string][]int{"Bad-Code": {1}}},
		"price code":    {Attributes: map[string][]int{"price": {1}}},
		"catalog code":  {Attributes: map[string][]int{"catalog": {1}}},
		"currency case": {Currency: "usd"},
	} {
		if _, err := p.query(); kindOf(err) != KindUsage {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	fq, err := SearchParams{CatalogIDs: []int{3026}, Attributes: map[string][]int{"video_game_rating": {170, 171}}, Order: "newest_first"}.filterQuery()
	if err != nil {
		t.Fatal(err)
	}
	if fq.Get("attribute_ids[video_game_rating]") != "170,171" || fq.Get("attribute_ids[catalog]") != "3026" || fq.Get("order") != "newest_first" {
		t.Fatalf("filter query = %v", fq)
	}
}

func TestItemDetails(t *testing.T) {
	_, c := newFake(t)
	it, err := c.Item(context.Background(), 1000000001)
	if err != nil {
		t.Fatal(err)
	}
	if it.BuyerProtectionFee != 3.85 || it.TotalPrice != 48.85 || it.BrandID != 53 || it.Color != "Black, White" || it.Uploaded != "2 hours ago" {
		t.Fatalf("item = %+v", it)
	}
	if it.CanBuy == nil || !*it.CanBuy {
		t.Fatalf("can_buy = %v", it.CanBuy)
	}
	if s := it.Shipping; s == nil || s.Price != 3.49 || s.Currency != "EUR" || s.Free || !s.FromPrice || s.Delivery != "home" {
		t.Fatalf("shipping = %+v", s)
	}
	// The brand crumb is dropped: it is the category filtered by brand.
	if want := []CategoryRef{{1904, "Women"}, {16, "Shoes"}, {1242, "Trainers"}}; !reflect.DeepEqual(it.CategoryPath, want) {
		t.Fatalf("category path = %+v", it.CategoryPath)
	}
	var attrs []string
	for _, a := range it.Attributes {
		attrs = append(attrs, a.Code+"="+a.Value)
	}
	if strings.Join(attrs, ";") != "brand=Nike;size=42;status=Very good;color=Black, White;material=Leather;upload_date=2 hours ago" {
		t.Fatalf("attributes = %v", attrs)
	}
	if len(it.Photos) != 2 || it.Photos[0] != (ItemPhoto{URL: it.Images[0], OriginalWidth: 1200, OriginalHeight: 1600, Main: true, DominantColor: "#CACAC9"}) || it.Photos[1].Main {
		t.Fatalf("photos = %+v", it.Photos)
	}
	s := it.Seller
	if s == nil || s.ID != 2000000001 || s.Login != "sample_seller" || s.Rating != 4.75 || s.Reviews != 4 || s.LastSeen != "Last seen 2 hours ago" ||
		!reflect.DeepEqual(s.Badges, []string{"ACTIVE_LISTER"}) || !reflect.DeepEqual(s.Info, []string{"Last seen 2 hours ago"}) || s.Profile != nil {
		t.Fatalf("seller = %+v", s)
	}
}

func TestItemSoldFreeShipping(t *testing.T) {
	c := New()
	it, err := c.parseItemPage(fixture(t, "item_sold.html"), 1000000001)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Sold || it.CanBuy == nil || *it.CanBuy || it.Shipping == nil || !it.Shipping.Free || it.Shipping.Price != 0 || it.Shipping.FromPrice {
		t.Fatalf("item = %+v shipping = %+v", it, it.Shipping)
	}
}

func TestShippingRule(t *testing.T) {
	money := func(a string) *rawMoney {
		m := &rawMoney{CurrencyCC: "EUR"}
		_ = m.Amount.UnmarshalJSON([]byte(a))
		return m
	}
	for name, tc := range map[string]struct {
		in   rawShipping
		want Shipping
	}{
		"free":     {rawShipping{OriginalPrice: money("10.74"), FinalPrice: money("0"), IsFree: true}, Shipping{Currency: "EUR", Free: true}},
		"from":     {rawShipping{OriginalPrice: money("8.59"), FinalPrice: money("0"), Multiple: true}, Shipping{Price: 8.59, Currency: "EUR", FromPrice: true}},
		"single":   {rawShipping{OriginalPrice: money("5"), FinalPrice: money("3.5")}, Shipping{Price: 3.5, Currency: "EUR"}},
		"no final": {rawShipping{OriginalPrice: money("5"), FinalPrice: money("0")}, Shipping{Price: 5, Currency: "EUR"}},
	} {
		if got := tc.in.shipping(); *got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, *got, tc.want)
		}
	}
}

func TestPhoto(t *testing.T) {
	f, c := newFake(t)
	webp := []byte("RIFF\x10\x00\x00\x00WEBPVP8 fake")
	f.override["GET /t/p1/f800/a.webp"] = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Accept"), "image/") || r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(webp)
	}
	f.override["GET /t/expired.webp"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden"))
	}
	f.override["GET /t/wall.webp"] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(fixture(t, "datadome_403.html"))
	}
	f.override["GET /t/html.webp"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>hi</html>"))
	}
	g := &countingGate{}
	c.Gate = g
	data, ct, err := c.Photo(context.Background(), c.Base+"/t/p1/f800/a.webp?s=sig")
	if err != nil || ct != "image/webp" || string(data) != string(webp) || g.n != 1 {
		t.Fatalf("data=%q ct=%q err=%v gate=%d", data, ct, err, g.n)
	}
	// A refused signature is an error of its own, not the bot protection.
	if _, _, err := c.Photo(context.Background(), c.Base+"/t/expired.webp"); err == nil || kindOf(err) == KindBlocked {
		t.Fatalf("expired: %v", err)
	}
	if _, _, err := c.Photo(context.Background(), c.Base+"/t/wall.webp"); kindOf(err) != KindBlocked {
		t.Fatalf("wall: %v", err)
	}
	if _, _, err := c.Photo(context.Background(), c.Base+"/t/html.webp"); kindOf(err) != KindAPIChanged {
		t.Fatalf("html: %v", err)
	}
	for _, bad := range []string{"https://evil.example/t/a.webp", "http://images1.vinted.net/t/a.webp", "file:///etc/passwd", "not a url"} {
		if _, _, err := c.Photo(context.Background(), bad); kindOf(err) != KindUsage {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

type countingGate struct{ n int }

func (g *countingGate) Wait(context.Context) error { g.n++; return nil }
func (g *countingGate) Block(string, time.Duration) (time.Duration, error) {
	return 0, nil
}
