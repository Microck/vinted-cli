package vinted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// rscPageOf wraps v as one flight row the way Next.js streams it: compact,
// since a newline ends a row.
func rscPageOf(t *testing.T, v string) []byte {
	t.Helper()
	var row bytes.Buffer
	if err := json.Compact(&row, []byte(v)); err != nil {
		t.Fatalf("row is not json: %v", err)
	}
	chunk, _ := json.Marshal("0:" + row.String() + "\n")
	return []byte(`<html><script>self.__next_f.push([1,` + string(chunk) + `])</script></html>`)
}

func TestCatalogKeepsOddEntriesAndFields(t *testing.T) {
	c := New()
	c.Base = "https://www.vinted.fr"
	page, err := c.parseCatalogPage(rscPageOf(t, `{"items":[
		{"id":"11","productItem":{"id":11,"title":"Odd","url":"/items/11-odd","favouriteCount":"3",
			"price":"6.0","totalItemPrice":{"amount":"7.0","currencyCode":"EUR"},"isPromoted":"yes",
			"thumbnailUrls":["https://img/t.webp"],"photos":{"main":"x"},"user":{"id":"22"},"itemBox":["Nike","M · Good"]}},
		"ad-slot",
		{"id":"12","productItem":"teaser"},
		{"id":"13","productItem":{"id":13,"title":"Plain","price":{"amount":"5.0","currencyCode":"EUR"},"itemBox":{"firstLine":"Adidas","secondLine":"L · Good"}}}
	],"pagination":{"current_page":1,"total_pages":"many"}}`))
	if err != nil {
		t.Fatal(err)
	}
	wantUnparsed(t, "page", page.Unparsed, "items.1", "pagination")
	if string(page.Unparsed["items.1"]) != `"ad-slot"` {
		t.Fatalf("items.1 = %s", page.Unparsed["items.1"])
	}
	if len(page.Items) != 3 {
		t.Fatalf("items = %+v", page.Items)
	}
	odd := page.Items[0]
	if odd.ID != 11 || odd.Title != "Odd" || odd.TotalPrice != 7 || odd.SellerID != 22 || odd.Favorites != 3 ||
		odd.URL != "https://www.vinted.fr/items/11-odd" || !reflect.DeepEqual(odd.Images, []string{"https://img/t.webp"}) {
		t.Fatalf("odd item = %+v", odd)
	}
	wantUnparsed(t, "odd item", odd.Unparsed, "productItem.isPromoted", "productItem.itemBox", "productItem.photos", "productItem.price")
	if string(odd.Unparsed["productItem.price"]) != `"6.0"` {
		t.Fatalf("price raw = %s", odd.Unparsed["productItem.price"])
	}
	teaser := page.Items[1]
	if teaser.ID != 12 || string(teaser.Unparsed["productItem"]) != `"teaser"` {
		t.Fatalf("teaser = %+v", teaser)
	}
	plain := page.Items[2]
	if plain.ID != 13 || plain.Price != 5 || plain.Brand != "Adidas" || plain.Size != "L" || plain.Unparsed != nil {
		t.Fatalf("plain = %+v", plain)
	}
}

func TestFiltersKeepOddEntriesAndFields(t *testing.T) {
	f, c := newFake(t)
	f.override["GET /web/gateway/svc-filters/filters"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"filters":[
			{"id":1,"code":"status","title":{"en":"Condition"},"display_type":"list","selection_type":7,"is_hidden":"no","is_lazy":false,
				"options":[{"id":6,"title":"New","type":"default","items_count":"12","prefix":"#fff"},{"id":9,"title":{"en":"Bad"}},{"id":2,"title":"Good","description":["a"]}]},
			{"id":2,"code":17},
			{"id":3,"code":"color","title":"Colour","display_type":"grid","selection_type":"multi","is_lazy":true,"options":[]}
		]}`))
	}
	f.override["GET /web/gateway/svc-filters/filters/facets"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter_code") != "color" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"options":[{"id":1,"title":"Black","prefix":{"hex":"#000","url":"b.png"}},"oops",{"id":"G","title":"Group","type":"group","options":"nope"}]}`))
	}
	filters, err := c.Filters(context.Background(), SearchParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != 3 {
		t.Fatalf("filters = %+v", filters)
	}
	status := filters[0]
	if status.Code != "status" || status.Type != "list" || status.Multi || status.Title != "" {
		t.Fatalf("status = %+v", status)
	}
	wantUnparsed(t, "status", status.Unparsed, "is_hidden", "options.1", "selection_type", "title")
	if len(status.Options) != 2 || status.Options[0].Count != 12 || status.Options[1].Title != "Good" {
		t.Fatalf("status options = %+v", status.Options)
	}
	wantUnparsed(t, "New", status.Options[0].Unparsed, "prefix")
	wantUnparsed(t, "Good", status.Options[1].Unparsed, "description")

	whole := filters[1]
	if whole.Code != "" || string(whole.Unparsed["filters.1"]) != `{"id":2,"code":17}` {
		t.Fatalf("unreadable filter = %+v", whole)
	}

	color := filters[2]
	wantUnparsed(t, "color", color.Unparsed, "facets.options.1")
	if len(color.Options) != 2 || color.Options[0].Hex != "#000" || color.Options[1].Type != "group" || color.Options[1].Selectable() {
		t.Fatalf("color options = %+v", color.Options)
	}
	if string(color.Options[1].Unparsed["options"]) != `"nope"` {
		t.Fatalf("group unparsed = %v", color.Options[1].Unparsed)
	}
}

func TestUploadAnswersKeepOddParts(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["POST /api/v2/item_upload/attributes"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"attributes":[
			{"code":"size","configuration":{"title":"Size","required":"yes","selection_type":"single","selection_limit":"1",
				"options":[{"id":1,"title":"S"},{"title":["bad"]},{"type":"group","title":"Kids","options":[{"id":5,"title":"K1"}]}]}},
			{"code":"material","configuration":"text-only"},
			{"code":42},
			{"code":"color","configuration":null}
		]}`))
	}
	attrs, err := c.UploadAttributes(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 3 {
		t.Fatalf("attributes = %+v", attrs)
	}
	size := attrs[0]
	if size.Code != "size" || size.Title != "Size" || size.Required || size.Limit != 1 ||
		!reflect.DeepEqual(size.Options, []AttributeOption{{ID: 1, Title: "S"}, {ID: 5, Title: "K1"}}) {
		t.Fatalf("size = %+v", size)
	}
	wantUnparsed(t, "size", size.Unparsed, "configuration.options.1", "configuration.required")
	if m := attrs[1]; m.Code != "material" || string(m.Unparsed["configuration"]) != `"text-only"` {
		t.Fatalf("material = %+v", m)
	}
	if w := attrs[2]; w.Code != "" || string(w.Unparsed["attributes.2"]) != `{"code":42}` {
		t.Fatalf("unreadable attribute = %+v", w)
	}

	f.override["POST /api/v2/photos"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"77","url":{"f800":"x"},"temp_uuid":"u-1"}`))
	}
	p, err := c.UploadPhoto(context.Background(), UploadImage{Name: "a.jpg", Data: []byte("\xff\xd8\xff")}, "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != 77 || p.URL != "" || p.TempUUID != "u-1" {
		t.Fatalf("photo = %+v", p)
	}
	wantUnparsed(t, "photo", p.Unparsed, "url")

	f.override["GET /web/gateway/shipping-estimation/external/catalogs/1/package_sizes"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"package_sizes":[{"id":1,"code":"S","title":"Small","name":{"x":1}}]}`))
	}
	sizes, err := c.PackageSizes(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[0].ID != 1 || sizes[0].Code != "S" || sizes[0].Title != "Small" {
		t.Fatalf("sizes = %+v", sizes)
	}
	wantUnparsed(t, "package size", sizes[0].Unparsed, "name")

	// A draft answered with an odd is_draft still reads as the draft.
	f.override["POST /api/v2/item_upload/drafts"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"draft":{"id":6,"is_draft":"maybe"}}`))
	}
	up, err := c.CreateItem(context.Background(), ItemInput{Title: "T", Price: 5, Currency: "EUR", Photos: []UploadImage{{Name: "a.jpg", Data: []byte("\xff\xd8\xff")}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if up.ID != 6 || !up.Draft || up.Title != "T" {
		t.Fatalf("draft = %+v", up)
	}
	wantUnparsed(t, "draft", up.Unparsed, "is_draft")
}

func TestUploadedItemInAnUnknownShapeKeepsItsID(t *testing.T) {
	c := New()
	// An item answer in a shape rawAPIItem does not read falls back to the
	// raw body, keeping the id.
	var ans uploadAnswer
	ans.Item.Raw = json.RawMessage(`{"id":5,"title":["T"]}`)
	up, err := c.uploadedItem("POST /api/v2/item_upload/items", ans)
	if err != nil {
		t.Fatal(err)
	}
	if up.ID != 5 || up.Draft || !strings.HasSuffix(up.URL, "/items/5") {
		t.Fatalf("item = %+v", up)
	}
	wantUnparsed(t, "item", up.Unparsed, "item")
	ans.Item.Raw = json.RawMessage(`{"title":"no id"}`)
	if _, err := c.uploadedItem("POST /api/v2/item_upload/items", ans); kindOf(err) != KindAPIChanged {
		t.Fatalf("no id: err = %v", err)
	}
}

func TestOffersReadConversationsWithOddMessages(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	f.override["GET /api/v2/conversations/6000000009"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"conversation":{"id":6000000009,
			"transaction":{"id":8,"user_side":5,"seller_id":3000000001,"offer_price":"45"},
			"messages":[
				"garbage",
				{"entity_type":"message","entity":{"body":"hi","price":{"weird":true},"currency":{"a":1}}},
				{"entity_type":"offer_request_message","entity":{"offer_request_id":5,"user_id":2000000001,"status":10,"price":"60.0","currency":"USD"}},
				{"entity_type":"offer_request_message","entity":"broken"}
			]}}`))
	}
	f.override["PUT /api/v2/transactions/8/offer_requests/5/accept"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}
	o, err := c.RespondOffer(context.Background(), 6000000009, true)
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != 5 || o.Amount != 60 || o.Currency != "USD" || o.Status != "accepted" || o.Transaction != 8 {
		t.Fatalf("offer = %+v", o)
	}
	wantUnparsed(t, "offer", o.Unparsed, "conversation.messages.0", "conversation.messages.3.entity",
		"conversation.transaction.offer_price", "conversation.transaction.user_side")

	// The offer is sent before its answer is read, so an odd answer is kept,
	// not turned into an error.
	f.override["POST /api/v2/transactions/8/offers"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"offer":"created"}`))
	}
	sent, err := c.SendOffer(context.Background(), 6000000009, 55, "")
	if err != nil {
		t.Fatal(err)
	}
	if sent.Kind != "offer" || sent.Currency != "USD" || string(sent.Unparsed["offer"]) != `"created"` {
		t.Fatalf("sent = %+v", sent)
	}

	f.override["GET /api/v2/conversations/6000000010"] = func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"conversation":{"transaction":"8"}}`))
	}
	if _, err := c.RespondOffer(context.Background(), 6000000010, true); kindOf(err) != KindAPIChanged {
		t.Fatalf("unreadable transaction: err = %v", err)
	}
}

func TestErrorEnvelopeInOddShapes(t *testing.T) {
	c := &Client{Base: DefaultBase}
	raw := []byte(`{"code":"99","message_code":"validation_error","errors":[{"field":"photos","value":["too small","blurry"]},"oops"]}`)
	err := c.statusError("POST /api/v2/item_upload/items", http.StatusUnprocessableEntity, raw)
	var ve *Error
	if !errors.As(err, &ve) || ve.Kind != KindUsage || ve.APICode != 99 || ve.Msg != `vinted rejected the input: photos: too small, blurry; "oops"` {
		t.Fatalf("err = %#v", err)
	}
	// A 403 in Vinted's own envelope is a refusal, not a block, even with
	// the gateway's string codes.
	if isBlock(http.StatusForbidden, []byte(`{"code":"FORBIDDEN","message":"Nope"}`)) {
		t.Fatal("gateway refusal read as a block")
	}
}
