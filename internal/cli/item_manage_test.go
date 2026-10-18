package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// listingFake fronts fakeVinted with the seller-side endpoints: the upload
// form, photo uploads, listing visibility and deletion, offers, reservations
// and the gateway's saved searches. Everything else falls through to the
// shared fake, so login and token refresh behave exactly as elsewhere.
type sellerFake struct {
	h   *harness
	srv *httptest.Server

	mu    sync.Mutex
	calls []sellerCall
	// verification makes every write answer code 115, as Vinted does for
	// accounts it has not verified.
	verification bool
	photos       int
}

type sellerCall struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

const (
	ownItemID      = int64(1000000002)
	draftID        = int64(1000000099)
	buyerConvID    = int64(6000000001)
	sellerConvID   = int64(6000000002)
	transactionID  = int64(8000000001)
	pendingOfferID = int64(900)
	savedSearchID  = int64(4650918777)
	recentSearchID = int64(4649077018)
)

func newSellerHarness(t *testing.T) (*harness, *sellerFake) {
	t.Helper()
	h := newHarness(t)
	lf := &sellerFake{h: h}
	lf.srv = httptest.NewServer(http.HandlerFunc(lf.serve))
	t.Cleanup(lf.srv.Close)
	t.Setenv("VINTED_BASE_URL", lf.srv.URL)
	h.login()
	return h, lf
}

func (lf *sellerFake) callsTo(method, path string) []sellerCall {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	var out []sellerCall
	for _, c := range lf.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

func (lf *sellerFake) writes() int {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	n := 0
	for _, c := range lf.calls {
		if c.Method != http.MethodGet && c.Path != "/web/api/auth/refresh" && c.Path != "/api/v2/item_upload/attributes" {
			n++
		}
	}
	return n
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (lf *sellerFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	lf.mu.Lock()
	lf.calls = append(lf.calls, sellerCall{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	verification := lf.verification
	lf.mu.Unlock()
	t := lf.h.t
	path := r.URL.Path
	auth := func() bool {
		if lf.h.fake.authorized(r) {
			return true
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":100,"message":"Invalid authentication token","message_code":"invalid_authentication_token"}`))
		return false
	}
	write := func() bool {
		if !auth() {
			return false
		}
		if r.Header.Get("X-CSRF-Token") != fakeCSRF {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":106,"message":"csrf"}`))
			return false
		}
		if verification {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(fixture(t, "error_115.json"))
			return false
		}
		return true
	}
	ownItem := fmt.Sprint(ownItemID)
	searches := fmt.Sprintf("/web/gateway/search-bar/v2/users/%d/previous_searches", fakeUserID)
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/items/"+ownItem):
		// The fixture item page, relisted under the account's own id.
		page := bytes.ReplaceAll(fixture(t, "item.html"), []byte("1000000001"), []byte(ownItem))
		page = bytes.ReplaceAll(page, []byte("2000000001"), []byte(fmt.Sprint(fakeUserID)))
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(page)
	case r.Method == http.MethodPost && path == "/api/v2/photos":
		if !write() {
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.FormValue("photo[type]") != "item" || r.FormValue("photo[temp_uuid]") == "" {
			http.Error(w, `{"code":99,"message":"bad photo form"}`, http.StatusUnprocessableEntity)
			return
		}
		if _, _, err := r.FormFile("photo[file]"); err != nil {
			http.Error(w, `{"code":99,"message":"no file"}`, http.StatusUnprocessableEntity)
			return
		}
		lf.mu.Lock()
		lf.photos++
		id := 5550 + lf.photos
		lf.mu.Unlock()
		writeJSON(w, map[string]any{"id": id, "url": fmt.Sprintf("https://images.example/%d.jpg", id), "temp_uuid": r.FormValue("photo[temp_uuid]")})
	case r.Method == http.MethodPost && path == "/api/v2/item_upload/attributes":
		if !auth() {
			return
		}
		writeJSON(w, map[string]any{"code": 0, "attributes": []any{
			map[string]any{"code": "brand", "configuration": nil},
			map[string]any{"id": 8001, "code": "size", "configuration": map[string]any{"title": "Size", "required": true, "selection_type": "single", "selection_limit": 1,
				"options": []any{map[string]any{"id": 32, "title": "Sizes", "type": "group", "options": []any{
					map[string]any{"id": 206, "title": "XS", "type": "default"}, map[string]any{"id": 207, "title": "S", "type": "default"}}}}}},
			map[string]any{"id": 431, "code": "condition", "configuration": map[string]any{"title": "Condition", "required": true, "selection_type": "single", "selection_limit": 1,
				"options": []any{map[string]any{"id": 1, "title": "Condition", "type": "group", "options": []any{
					map[string]any{"id": 6, "title": "New with tags", "type": "default"}, map[string]any{"id": 2, "title": "Very good", "type": "default"}}}}}},
			map[string]any{"id": 8, "code": "material", "configuration": map[string]any{"title": "Material", "required": false, "selection_type": "multi", "selection_limit": 3,
				"options": []any{map[string]any{"id": 1, "title": "Material", "type": "group", "options": []any{
					map[string]any{"id": 44, "title": "Cotton", "type": "default"}, map[string]any{"id": 45, "title": "Wool", "type": "default"}}}}}},
		}})
	case r.Method == http.MethodGet && path == "/api/v2/item_upload/colors":
		if auth() {
			writeJSON(w, map[string]any{"colors": []any{map[string]any{"id": 1, "title": "Black"}, map[string]any{"id": 12, "title": "White"}}})
		}
	case r.Method == http.MethodGet && path == "/api/v2/item_upload/brands":
		if auth() {
			writeJSON(w, map[string]any{"brands": []any{map[string]any{"id": 53, "title": "Nike"}, map[string]any{"id": 5977, "title": "Nike Air"}}})
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/web/gateway/shipping-estimation/external/catalogs/"):
		if !auth() {
			return
		}
		if r.Header.Get("Locale") == "" {
			http.Error(w, `{"code":"BAD_REQUEST","message":"Bad request"}`, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"package_sizes": []any{
			map[string]any{"id": 1, "code": "SMALL", "title": "Small"}, map[string]any{"id": 2, "code": "MEDIUM", "title": "Medium"}}})
	case r.Method == http.MethodPost && (path == "/api/v2/item_upload/items" || path == "/api/v2/item_upload/drafts"):
		if !write() {
			return
		}
		var in map[string]map[string]any
		_ = json.Unmarshal(body, &in)
		key, id := "item", ownItemID
		if strings.HasSuffix(path, "drafts") {
			key, id = "draft", draftID
		}
		form := in[key]
		writeJSON(w, map[string]any{key: map[string]any{
			"id": id, "title": form["title"], "user_id": fakeUserID, "is_draft": key == "draft",
			"price": map[string]any{"amount": fmt.Sprint(form["price"]), "currency_code": form["currency"]},
			"url":   lf.srv.URL + "/items/" + fmt.Sprint(id) + "-listing",
		}})
	case r.Method == http.MethodGet && path == "/api/v2/item_upload/items/"+ownItem:
		if auth() {
			writeJSON(w, map[string]any{"item": map[string]any{
				"id": ownItemID, "title": "Old title", "description": "Old description", "catalog_id": 1242,
				"price": map[string]any{"amount": "40.0", "currency_code": "EUR"}, "brand_id": 53, "brand_title": "Nike",
				"color_ids": []int{1}, "package_size_id": 2, "is_unisex": false,
				"photos":          []any{map[string]any{"id": 777, "url": "https://images.example/777.jpg"}},
				"item_attributes": []any{map[string]any{"code": "size", "ids": []int{207}}, map[string]any{"code": "condition", "ids": []int{6}}},
			}})
		}
	case r.Method == http.MethodPut && path == "/api/v2/item_upload/items/"+ownItem:
		if !write() {
			return
		}
		var in map[string]map[string]any
		_ = json.Unmarshal(body, &in)
		form := in["item"]
		writeJSON(w, map[string]any{"item": map[string]any{
			"id": ownItemID, "title": form["title"], "user_id": fakeUserID,
			"price": map[string]any{"amount": fmt.Sprint(form["price"]), "currency_code": form["currency"]},
		}})
	case path == "/api/v2/items/"+ownItem+"/is_hidden" && r.Method == http.MethodPut,
		path == "/api/v2/items/"+ownItem+"/delete" && r.Method == http.MethodPost,
		path == fmt.Sprintf("/api/v2/item_upload/drafts/%d", draftID) && r.Method == http.MethodDelete:
		if write() {
			writeJSON(w, map[string]any{"code": 0})
		}
	case r.Method == http.MethodGet && path == fmt.Sprintf("/api/v2/conversations/%d", sellerConvID):
		if !auth() {
			return
		}
		var conv map[string]map[string]any
		_ = json.Unmarshal(fixture(t, "conversation.json"), &conv)
		c := conv["conversation"]
		c["id"] = sellerConvID
		c["transaction"].(map[string]any)["user_side"] = "seller"
		c["messages"] = append(c["messages"].([]any), map[string]any{
			"entity_type": "offer_request_message", "created_at_ts": "2026-10-17T12:05:00+02:00",
			"entity": map[string]any{"offer_request_id": pendingOfferID, "user_id": 2000000001, "status": 10, "price": "30.0", "currency": "EUR", "transaction_id": transactionID},
		})
		writeJSON(w, conv)
	case strings.HasPrefix(path, fmt.Sprintf("/api/v2/transactions/%d/", transactionID)):
		if !write() {
			return
		}
		switch strings.TrimPrefix(path, fmt.Sprintf("/api/v2/transactions/%d/", transactionID)) {
		case "offer_requests":
			writeJSON(w, map[string]any{"offer_request": map[string]any{"id": 901}})
		case "offers":
			writeJSON(w, map[string]any{"offer": map[string]any{"id": 902}})
		case fmt.Sprintf("offer_requests/%d/accept", pendingOfferID), fmt.Sprintf("offer_requests/%d/reject", pendingOfferID), "reservation":
			writeJSON(w, map[string]any{"code": 0})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":104,"message":"Content not found","message_code":"not_found"}`))
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, searches):
		if !auth() {
			return
		}
		if r.Header.Get("Locale") == "" {
			http.Error(w, `{"code":"BAD_REQUEST","message":"Bad request"}`, http.StatusBadRequest)
			return
		}
		saved := map[string]any{"id": savedSearchID, "title": "levis 501", "subtitle": "Men, Levi's, To 30.00 EUR", "type": "type_saved",
			"search_text": "levis 501", "catalog_id": 5, "price_to": map[string]any{"amount": "30.00", "currency_code": "EUR"}, "new_items_count": 4,
			"selected_filters": []any{map[string]any{"code": "brand", "ids": []string{"10"}}, map[string]any{"code": "status", "ids": []string{"2", "3"}}, map[string]any{"code": "patterns", "ids": []string{"7"}}}}
		recent := map[string]any{"id": recentSearchID, "title": "nike", "subtitle": "No filters", "type": "type_recent", "search_text": "nike", "selected_filters": []any{}}
		switch strings.TrimPrefix(path, searches) {
		case "":
			writeJSON(w, map[string]any{"previous_searches": []any{saved, recent}})
		case fmt.Sprintf("/%d", savedSearchID):
			writeJSON(w, map[string]any{"previous_search": saved})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		lf.h.fake.serve(w, r)
	}
}

func testImage(t *testing.T) string {
	t.Helper()
	// A 1x1 PNG: enough for content sniffing.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")
	p := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(p, png, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type uploadedOut struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	Draft    bool    `json:"draft"`
	URL      string  `json:"url"`
}

func createArgs(img string, extra ...string) []string {
	return append([]string{"item", "create", "--title", "Air Max", "--description", "Worn twice", "--price", "45.5",
		"--category", "Trainers", "--condition", "very good", "--size", "S", "--brand", "nike", "--color", "Black",
		"--attr", "material=Cotton", "--attr", "material=44", "--image", img, "--image", img, "--currency", "eur"}, extra...)
}

func TestItemCreateUploadsPhotosAndPublishes(t *testing.T) {
	h, lf := newSellerHarness(t)
	img := testImage(t)
	out := decode[uploadedOut](t, h.must("", createArgs(img)...).stdout)
	if out.ID != ownItemID || out.Title != "Air Max" || out.Price != 45.5 || out.Currency != "EUR" || out.Draft {
		t.Fatalf("listing = %+v", out)
	}
	photos := lf.callsTo(http.MethodPost, "/api/v2/photos")
	if len(photos) != 2 {
		t.Fatalf("photo uploads = %d, want 2", len(photos))
	}
	calls := lf.callsTo(http.MethodPost, "/api/v2/item_upload/items")
	if len(calls) != 1 {
		t.Fatalf("create calls = %d", len(calls))
	}
	if calls[0].Header.Get("X-Upload-Form") != "true" {
		t.Fatalf("create lacks X-Upload-Form header")
	}
	var body struct {
		Item struct {
			Title       string           `json:"title"`
			Price       float64          `json:"price"`
			Currency    string           `json:"currency"`
			CatalogID   int              `json:"catalog_id"`
			BrandID     int              `json:"brand_id"`
			Brand       string           `json:"brand"`
			ColorIDs    []int            `json:"color_ids"`
			PackageSize int              `json:"package_size_id"`
			TempUUID    string           `json:"temp_uuid"`
			Photos      []map[string]int `json:"assigned_photos"`
			Attrs       []struct {
				Code string `json:"code"`
				IDs  []int  `json:"ids"`
			} `json:"item_attributes"`
		} `json:"item"`
		PushUp  bool   `json:"push_up"`
		Session string `json:"upload_session_id"`
	}
	if err := json.Unmarshal(calls[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	it := body.Item
	if it.Title != "Air Max" || it.Price != 45.5 || it.Currency != "EUR" || it.CatalogID != 1242 || it.BrandID != 53 || it.Brand != "Nike" || body.Session == "" || it.TempUUID == "" {
		t.Fatalf("item payload = %s", calls[0].Body)
	}
	if len(it.ColorIDs) != 1 || it.ColorIDs[0] != 1 || it.PackageSize != 1 {
		t.Fatalf("colors/package = %v/%d", it.ColorIDs, it.PackageSize)
	}
	if len(it.Photos) != 2 || it.Photos[0]["id"] != 5551 || it.Photos[1]["id"] != 5552 {
		t.Fatalf("assigned photos = %v", it.Photos)
	}
	got := map[string][]int{}
	for _, a := range it.Attrs {
		got[a.Code] = a.IDs
	}
	if fmt.Sprint(got) != fmt.Sprint(map[string][]int{"condition": {2}, "size": {207}, "material": {44, 44}}) {
		t.Fatalf("attributes = %v", got)
	}
	// Both photos were uploaded under the item's temp uuid.
	for _, p := range photos {
		if !bytes.Contains(p.Body, []byte(it.TempUUID)) {
			t.Fatalf("photo upload not tied to temp uuid %s", it.TempUUID)
		}
	}
}

func TestItemCreateDraft(t *testing.T) {
	h, lf := newSellerHarness(t)
	out := decode[uploadedOut](t, h.must("", createArgs(testImage(t), "--draft", "--package-size", "medium")...).stdout)
	if out.ID != draftID || !out.Draft {
		t.Fatalf("draft = %+v", out)
	}
	calls := lf.callsTo(http.MethodPost, "/api/v2/item_upload/drafts")
	if len(calls) != 1 || !bytes.Contains(calls[0].Body, []byte(`"package_size_id":2`)) {
		t.Fatalf("draft calls = %d %s", len(calls), calls)
	}
	if n := len(lf.callsTo(http.MethodPost, "/api/v2/item_upload/items")); n != 0 {
		t.Fatalf("a draft must not publish, got %d publish calls", n)
	}
}

func TestItemCreateValidatesBeforeUploading(t *testing.T) {
	h, lf := newSellerHarness(t)
	img := testImage(t)
	notImage := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notImage, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sparse file over the cap: refused from its size, never read whole.
	huge := filepath.Join(t.TempDir(), "huge.png")
	if err := os.WriteFile(huge, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(huge, 32<<20+1); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"item", "create", "--title", "x"}, "missing required --description, --price, --category, --condition, --image FILE"},
		{createArgs(img, "--size", "XXL"), `"XXL" is not a valid size`},
		{createArgs(img, "--attr", "pattern=Striped"), `has no "pattern" attribute`},
		{createArgs(img, "--attr", "size=S"), "set size with --size"},
		{createArgs(img, "--color", "Mauve"), `"Mauve" is not a valid color`},
		{createArgs(img, "--brand", "Nikey"), `no brand named "Nikey". Close matches: Nike, Nike Air`},
		{createArgs(img, "--category", "Women"), "has subcategories"},
		{createArgs(img, "--category", "Nope"), `no leaf category named "Nope"`},
		{createArgs(img, "--package-size", "huge"), `"huge" is not a valid package-size`},
		{createArgs(img, "--price", "0"), "--price must be positive"},
		{createArgs(notImage), "is not an image"},
		{createArgs(huge), "is larger than 32 MiB"},
		{[]string{"item", "create", "--title", "x", "--description", "y", "--price", "3", "--category", "1242", "--condition", "Very good", "--image", img, "--currency", "EUR"}, "category 1242 requires --size"},
	} {
		r := h.run("", tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
			t.Fatalf("%v: exit %d, stderr %q, want %q", tc.args, r.code, r.stderr, tc.want)
		}
	}
	if n := lf.writes(); n != 0 {
		t.Fatalf("validation failures still wrote %d times", n)
	}
}

func TestItemCreateUnverifiedAccountIsAuthError(t *testing.T) {
	h, lf := newSellerHarness(t)
	lf.verification = true
	r := h.run("", createArgs(testImage(t))...)
	if r.code != 3 || !strings.Contains(r.stderr, "verified") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
}

func TestItemEditKeepsUntouchedFields(t *testing.T) {
	h, lf := newSellerHarness(t)
	out := decode[uploadedOut](t, h.must("", "item", "edit", fmt.Sprint(ownItemID), "--price", "39", "--condition", "New with tags", "--image", testImage(t)).stdout)
	if out.ID != ownItemID || out.Price != 39 {
		t.Fatalf("edit = %+v", out)
	}
	calls := lf.callsTo(http.MethodPut, fmt.Sprintf("/api/v2/item_upload/items/%d", ownItemID))
	if len(calls) != 1 {
		t.Fatalf("edit calls = %d", len(calls))
	}
	var body struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal(calls[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	it := body.Item
	if it["title"] != "Old title" || it["description"] != "Old description" || it["price"] != 39.0 || it["currency"] != "EUR" || it["brand"] != "Nike" || it["catalog_id"] != 1242.0 {
		t.Fatalf("edit payload = %s", calls[0].Body)
	}
	attrs, _ := json.Marshal(it["item_attributes"])
	if string(attrs) != `[{"code":"size","ids":[207]},{"code":"condition","ids":[6]}]` {
		t.Fatalf("attributes = %s", attrs)
	}
	photos, _ := json.Marshal(it["assigned_photos"])
	if string(photos) != `[{"id":5551,"orientation":0}]` {
		t.Fatalf("photos = %s", photos)
	}
	if r := h.run("", "item", "edit", fmt.Sprint(ownItemID)); r.code != 2 || !strings.Contains(r.stderr, "nothing to change") {
		t.Fatalf("edit without flags: exit %d %q", r.code, r.stderr)
	}
}

func TestItemEditWithoutPhotosKeepsGallery(t *testing.T) {
	h, lf := newSellerHarness(t)
	h.must("", "item", "edit", fmt.Sprint(ownItemID), "--title", "New title")
	calls := lf.callsTo(http.MethodPut, fmt.Sprintf("/api/v2/item_upload/items/%d", ownItemID))
	if len(calls) != 1 || !bytes.Contains(calls[0].Body, []byte(`"assigned_photos":[{"id":777,"orientation":0}]`)) || !bytes.Contains(calls[0].Body, []byte(`"title":"New title"`)) {
		t.Fatalf("edit payload = %s", calls)
	}
	if n := len(lf.callsTo(http.MethodPost, "/api/v2/photos")); n != 0 {
		t.Fatalf("edit without --image uploaded %d photos", n)
	}
}

func TestItemDeleteHideAndOwnership(t *testing.T) {
	h, lf := newSellerHarness(t)
	own := fmt.Sprint(ownItemID)
	// Someone else's item is refused before any write.
	if r := h.run("", "item", "delete", fixtureItemRef(), "--yes"); r.code != 2 || !strings.Contains(r.stderr, "belongs to another member") {
		t.Fatalf("foreign delete: exit %d %q", r.code, r.stderr)
	}
	if r := h.run("", "item", "hide", fixtureItemRef()); r.code != 2 {
		t.Fatalf("foreign hide: exit %d", r.code)
	}
	// Non-interactive delete without --yes is a usage error, not a delete.
	if r := h.run("", "item", "delete", own); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("delete without --yes: exit %d %q", r.code, r.stderr)
	}
	if n := lf.writes(); n != 0 {
		t.Fatalf("refused commands wrote %d times", n)
	}
	h.must("", "item", "hide", own)
	h.must("", "item", "unhide", own)
	hides := lf.callsTo(http.MethodPut, "/api/v2/items/"+own+"/is_hidden")
	if len(hides) != 2 || string(hides[0].Body) != `{"is_hidden":true}` || string(hides[1].Body) != `{"is_hidden":false}` {
		t.Fatalf("hide calls = %v", hides)
	}
	out := decode[actionOut](t, h.must("", "item", "delete", own, "--yes").stdout)
	if out.Action != "deleted" || out.Item != ownItemID || len(lf.callsTo(http.MethodPost, "/api/v2/items/"+own+"/delete")) != 1 {
		t.Fatalf("delete = %+v", out)
	}
	h.must("", "item", "delete", fmt.Sprint(draftID), "--draft", "--yes")
	if len(lf.callsTo(http.MethodDelete, fmt.Sprintf("/api/v2/item_upload/drafts/%d", draftID))) != 1 {
		t.Fatal("draft was not deleted")
	}
}

type actionOut struct {
	Action string `json:"action"`
	Item   int64  `json:"item"`
	OK     bool   `json:"ok"`
}

type offerOut struct {
	ID           int64   `json:"id"`
	Conversation int64   `json:"conversation"`
	Transaction  int64   `json:"transaction"`
	Amount       float64 `json:"amount"`
	Currency     string  `json:"currency"`
	Status       string  `json:"status"`
	Kind         string  `json:"kind"`
}

func TestChatOfferAsBuyerAndSeller(t *testing.T) {
	h, lf := newSellerHarness(t)
	buyer := decode[offerOut](t, h.must("", "chat", "offer", fmt.Sprint(buyerConvID), "40.5").stdout)
	if buyer.Kind != "request" || buyer.ID != 901 || buyer.Currency != "EUR" || buyer.Amount != 40.5 || buyer.Transaction != transactionID {
		t.Fatalf("buyer offer = %+v", buyer)
	}
	reqs := lf.callsTo(http.MethodPost, fmt.Sprintf("/api/v2/transactions/%d/offer_requests", transactionID))
	if len(reqs) != 1 || string(reqs[0].Body) != `{"offer_request":{"currency":"EUR","price":"40.50"}}` {
		t.Fatalf("offer request body = %v", reqs)
	}
	seller := decode[offerOut](t, h.must("", "chat", "offer", fmt.Sprint(sellerConvID), "35").stdout)
	if seller.Kind != "offer" || seller.ID != 902 {
		t.Fatalf("seller offer = %+v", seller)
	}
	acc := decode[offerOut](t, h.must("", "chat", "offer", "accept", fmt.Sprint(sellerConvID)).stdout)
	if acc.ID != pendingOfferID || acc.Status != "accepted" || acc.Amount != 30 {
		t.Fatalf("accept = %+v", acc)
	}
	h.must("", "chat", "offer", "decline", fmt.Sprint(sellerConvID))
	if len(lf.callsTo(http.MethodPut, fmt.Sprintf("/api/v2/transactions/%d/offer_requests/%d/accept", transactionID, pendingOfferID))) != 1 ||
		len(lf.callsTo(http.MethodPut, fmt.Sprintf("/api/v2/transactions/%d/offer_requests/%d/reject", transactionID, pendingOfferID))) != 1 {
		t.Fatal("accept/reject not sent")
	}
	// Buyers cannot answer offers; bad amounts never reach the network.
	if r := h.run("", "chat", "offer", "accept", fmt.Sprint(buyerConvID)); r.code != 2 {
		t.Fatalf("buyer accept: exit %d %q", r.code, r.stderr)
	}
	for _, amount := range []string{"0", "-3", "1.234", "abc"} {
		if r := h.run("", "chat", "offer", fmt.Sprint(buyerConvID), amount); r.code != 2 {
			t.Fatalf("amount %s: exit %d", amount, r.code)
		}
	}
}

func TestChatReserve(t *testing.T) {
	h, lf := newSellerHarness(t)
	h.must("", "chat", "reserve", fmt.Sprint(sellerConvID))
	h.must("", "chat", "reserve", fmt.Sprint(sellerConvID), "--off")
	calls := lf.callsTo(http.MethodPut, fmt.Sprintf("/api/v2/transactions/%d/reservation", transactionID))
	if len(calls) != 2 || string(calls[0].Body) != `{"reserved":true}` || string(calls[1].Body) != `{"reserved":false}` {
		t.Fatalf("reservation calls = %v", calls)
	}
	if r := h.run("", "chat", "reserve", fmt.Sprint(buyerConvID)); r.code != 2 || !strings.Contains(r.stderr, "only the seller") {
		t.Fatalf("buyer reserve: exit %d %q", r.code, r.stderr)
	}
}

func TestChatOpenPrintsHistoryAndSendsLines(t *testing.T) {
	h, _ := newSellerHarness(t)
	r := h.must("first line\n\nsecond line\n/quit\nnot sent\n", "chat", "open", fmt.Sprint(buyerConvID), "--format", "pretty")
	if !strings.Contains(r.stdout, "Yes it is") || !strings.Contains(r.stdout, "me: first line") || !strings.Contains(r.stdout, "me: second line") {
		t.Fatalf("chat open output:\n%s", r.stdout)
	}
	h.fake.mu.Lock()
	replies := append([]string(nil), h.fake.replies...)
	h.fake.mu.Unlock()
	if len(replies) != 2 || !strings.Contains(replies[0], "first line") || !strings.Contains(replies[1], "second line") {
		t.Fatalf("replies = %v", replies)
	}
	if r := h.run("", "chat", "open", fmt.Sprint(buyerConvID), "--interval", "10ms"); r.code != 2 {
		t.Fatalf("tiny interval: exit %d", r.code)
	}
}

func TestAlertListAndWatchFromAlert(t *testing.T) {
	h, lf := newSellerHarness(t)
	type alertOut struct {
		ID    int64 `json:"id"`
		Saved bool  `json:"saved"`
		Query struct {
			Text       string           `json:"text"`
			CatalogIDs []int            `json:"catalog_ids"`
			BrandIDs   []int            `json:"brand_ids"`
			StatusIDs  []int            `json:"status_ids"`
			PriceTo    float64          `json:"price_to"`
			Attributes map[string][]int `json:"attributes"`
		} `json:"query"`
	}
	saved := decode[[]alertOut](t, h.must("", "alert", "list").stdout)
	if len(saved) != 1 || saved[0].ID != savedSearchID || !saved[0].Saved || saved[0].Query.Text != "levis 501" ||
		fmt.Sprint(saved[0].Query.CatalogIDs, saved[0].Query.BrandIDs, saved[0].Query.StatusIDs, saved[0].Query.Attributes) != "[5] [10] [2 3] map[patterns:[7]]" || saved[0].Query.PriceTo != 30 {
		t.Fatalf("alert list = %+v", saved)
	}
	if all := decode[[]alertOut](t, h.must("", "alert", "list", "--all").stdout); len(all) != 2 {
		t.Fatalf("alert list --all = %+v", all)
	}
	h.must("", "watch", "add", "search", "--from-alert", fmt.Sprint(savedSearchID), "--name", "levis")
	if r := h.must("", "watch", "list"); !strings.Contains(r.stdout, `"levis 501"`) || !strings.Contains(r.stdout, `"status_ids"`) {
		t.Fatalf("watch list:\n%s", r.stdout)
	}
	// A filter without its own field replays as CODE_ids[], as the site sends it.
	catalog := h.fake.requestsTo("/catalog")
	if len(catalog) == 0 || catalog[len(catalog)-1].Query.Get("patterns_ids[]") != "7" {
		t.Fatalf("the saved search's patterns filter was not sent: %v", catalog)
	}
	if r := h.run("", "watch", "add", "search", "levis", "--from-alert", fmt.Sprint(savedSearchID), "--name", "mixed"); r.code != 2 {
		t.Fatalf("text plus --from-alert: exit %d", r.code)
	}
	if r := h.run("", "watch", "add", "search", "--price-to", "3", "--from-alert", fmt.Sprint(savedSearchID), "--name", "mixed"); r.code != 2 {
		t.Fatalf("flags plus --from-alert: exit %d", r.code)
	}
	if r := h.run("", "watch", "add", "search", "--from-alert=", "--name", "empty"); r.code != 2 {
		t.Fatalf("empty --from-alert: exit %d", r.code)
	}
	if r := h.run("", "watch", "add", "search", "--from-alert", "123", "--name", "nope"); r.code != 4 {
		t.Fatalf("unknown saved search: exit %d %q", r.code, r.stderr)
	}
	for _, c := range lf.callsTo(http.MethodGet, fmt.Sprintf("/web/gateway/search-bar/v2/users/%d/previous_searches", fakeUserID)) {
		if c.Header.Get("Locale") == "" {
			t.Fatal("gateway call without Locale header")
		}
	}
}

func TestMCPAlertList(t *testing.T) {
	h, _ := newSellerHarness(t)
	s := h.mcpServer()
	text, isErr := s.callTool("alert_list", map[string]any{"all": true})
	if isErr || !strings.Contains(text, `"levis 501"`) || !strings.Contains(text, `"nike"`) {
		t.Fatalf("alert_list: err=%v %s", isErr, text)
	}
}
