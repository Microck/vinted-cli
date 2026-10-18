package vinted

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

// These tests feed the users, wardrobe, chat and saved-search decoders
// payloads whose optional parts have shapes the client does not know. The
// core must still be read, and every odd part must reach the output's
// Unparsed under its JSON path instead of failing the call.

func serveJSON(f *fakeVinted, key, body string) {
	f.override[key] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func wantUnparsed(t *testing.T, what string, got map[string]json.RawMessage, keys ...string) {
	t.Helper()
	have := make([]string, 0, len(got))
	for k := range got {
		have = append(have, k)
	}
	for _, k := range keys {
		if len(got[k]) == 0 {
			t.Errorf("%s: unparsed %q missing (have %v)", what, k, have)
		}
	}
	if len(got) != len(keys) {
		t.Errorf("%s: unparsed keys = %v, want %v", what, have, keys)
	}
}

func TestUserOddShapes(t *testing.T) {
	f, c := newFake(t)
	serveJSON(f, "GET /api/v2/users/2000000001", `{"user":{"id":"2000000001","login":"sample_seller","item_count":"2","feedback_count":4.0,
		"followers_count":"7","verification":{"email":"yes","phone":[true]}}}`)
	u, err := c.User(context.Background(), 2000000001)
	if err != nil || u.ID != 2000000001 || u.Login != "sample_seller" || u.Items != 2 || u.Reviews != 4 || u.Followers != 7 ||
		u.EmailVerified || u.PhoneVerified != nil {
		t.Fatalf("user=%+v err=%v", u, err)
	}
	wantUnparsed(t, "user", u.Unparsed, "verification.email", "verification.phone")
	if string(u.Unparsed["verification.email"]) != `"yes"` {
		t.Errorf("verification.email = %s", u.Unparsed["verification.email"])
	}

	serveJSON(f, "GET /api/v2/users/2000000001", `{"user":{"id":2000000001,"login":"sample_seller","verification":"pending"}}`)
	if u, err = c.User(context.Background(), 2000000001); err != nil || u.Login != "sample_seller" {
		t.Fatalf("user=%+v err=%v", u, err)
	}
	wantUnparsed(t, "user", u.Unparsed, "verification")

	// Without its id the profile is not a profile: the API changed.
	serveJSON(f, "GET /api/v2/users/2000000001", `{"user":{"login":"sample_seller"}}`)
	if _, err := c.User(context.Background(), 2000000001); kindOf(err) != KindAPIChanged {
		t.Fatalf("user without id err = %v", err)
	}
	loggedIn(c)
	serveJSON(f, "GET /api/v2/users/current", `{"user":{"id":null}}`)
	if _, err := c.Me(context.Background()); kindOf(err) != KindAPIChanged {
		t.Fatalf("me without id err = %v", err)
	}
}

func TestWardrobeOddShapes(t *testing.T) {
	f, c := newFake(t)
	serveJSON(f, "GET /api/v2/wardrobe/2000000001/items", `{"items":[
		{"id":1000000001,"title":"Nike Air Max 90","price":"6.0","currency":"EUR","total_item_price":["6.9"],"favourite_count":"3",
		 "brand":{"id":53,"title":"Nike"},"brand_title":"Nike","size":"42","status":{"id":2},
		 "photos":{"url":"https://images1.vinted.net/x.webp"},"photo":{"url":"https://images1.vinted.net/m.webp","is_main":true},
		 "user":"sample_seller","user_id":"2000000001"},
		"not an item"
	],"pagination":"none"}`)
	page, err := c.UserItems(context.Background(), 2000000001, 1, 20)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	it := page.Items[0]
	if it.ID != 1000000001 || it.Price != 6 || it.Currency != "EUR" || it.TotalPrice != 0 || it.Favorites != 3 || it.Brand != "Nike" ||
		it.Size != "42" || it.Condition != "" || it.SellerID != 2000000001 || len(it.Images) != 1 || it.Images[0] != "https://images1.vinted.net/m.webp" {
		t.Fatalf("item = %+v", it)
	}
	wantUnparsed(t, "item", it.Unparsed, "total_item_price", "brand", "status", "photos", "user")
	wantUnparsed(t, "page", page.Unparsed, "items.1", "pagination")
	if string(page.Unparsed["items.1"]) != `"not an item"` {
		t.Errorf("items.1 = %s", page.Unparsed["items.1"])
	}
}

func TestReviewsOddShapes(t *testing.T) {
	f, c := newFake(t)
	serveJSON(f, "GET /api/v2/user_feedbacks", `{"user_feedbacks":[
		{"id":5000000001,"rating":"5","feedback":"Fast shipping","created_at_ts":"2026-10-15T13:58:09+02:00","feedback_user_id":4000000001,"user":"anonymous"},
		42
	]}`)
	reviews, err := c.Reviews(context.Background(), 2000000001, 1)
	if err != nil || len(reviews) != 2 {
		t.Fatalf("reviews=%+v err=%v", reviews, err)
	}
	if r := reviews[0]; r.ID != 5000000001 || r.Rating != 5 || r.Comment != "Fast shipping" || r.ByID != 4000000001 || r.Date.IsZero() {
		t.Fatalf("review = %+v", r)
	}
	wantUnparsed(t, "review", reviews[0].Unparsed, "user")
	wantUnparsed(t, "unreadable review", reviews[1].Unparsed, "user_feedbacks.1")
}

func TestCategoriesAndBrandsOddShapes(t *testing.T) {
	f, c := newFake(t)
	serveJSON(f, "GET /api/v2/item_upload/catalogs", `{"catalogs":[
		{"id":"1904","title":"Women","catalogs":[{"id":16,"title":"Shoes","catalogs":[]},["odd"]]},
		"odd"
	]}`)
	cats, err := c.Categories(context.Background())
	if err != nil || len(cats) != 2 || cats[0].ID != 1904 || len(cats[0].Children) != 1 || cats[0].Children[0].ID != 16 {
		t.Fatalf("cats=%+v err=%v", cats, err)
	}
	wantUnparsed(t, "category", cats[0].Unparsed, "catalogs.1")
	wantUnparsed(t, "unreadable category", cats[1].Unparsed, "catalogs.1")

	serveJSON(f, "GET /api/v2/brands", `{"brands":[{"id":"53","title":"Nike","slug":"nike","item_count":"10"},"nike"]}`)
	brands, err := c.Brands(context.Background(), "nik")
	if err != nil || len(brands) != 2 || brands[0].ID != 53 || brands[0].Items != 10 || brands[0].Unparsed != nil {
		t.Fatalf("brands=%+v err=%v", brands, err)
	}
	wantUnparsed(t, "unreadable brand", brands[1].Unparsed, "brands.1")
}

func TestChatOddShapes(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	ctx := context.Background()
	serveJSON(f, "GET /api/v2/inbox", `{"conversations":[
		{"id":6000000001,"description":"Hi","unread":true,"opposite_user":"deleted user"},
		["odd"]
	]}`)
	inbox, err := c.Inbox(ctx, 1, 20)
	if err != nil || len(inbox) != 2 || inbox[0].ID != 6000000001 || inbox[0].LastMessage != "Hi" || !inbox[0].Unread {
		t.Fatalf("inbox=%+v err=%v", inbox, err)
	}
	wantUnparsed(t, "inbox row", inbox[0].Unparsed, "opposite_user")
	wantUnparsed(t, "unreadable inbox row", inbox[1].Unparsed, "conversations.1")

	serveJSON(f, "GET /api/v2/conversations/6000000001", `{"conversation":{"id":6000000001,"opposite_user":{"id":2000000001,"login":"sample_seller"},
		"transaction":{"item_id":1000000001,"item_title":"Nike","offer_price":["45"]},
		"messages":[
			{"entity_type":"message","created_at_ts":"2026-10-17T11:00:00+02:00","entity":{"id":7000000001,"body":"Hi","user_id":3000000001}},
			{"id":7000000002,"entity_type":"poll_message","created_at_ts":"2026-10-17T11:01:00+02:00","entity":"a poll"},
			7
		]}}`)
	conv, err := c.Conversation(ctx, 6000000001)
	if err != nil || conv.WithUser.Login != "sample_seller" || conv.Item.ID != 1000000001 || conv.Item.Price != 0 || len(conv.Messages) != 2 {
		t.Fatalf("conv=%+v err=%v", conv, err)
	}
	if m := conv.Messages[0]; m.Text != "Hi" || !m.FromSelf || m.Unparsed != nil {
		t.Fatalf("message 0 = %+v", m)
	}
	if m := conv.Messages[1]; m.ID != 7000000002 || m.Text != "" || m.At.IsZero() {
		t.Fatalf("message 1 = %+v", m)
	}
	wantUnparsed(t, "message", conv.Messages[1].Unparsed, "entity")
	wantUnparsed(t, "conversation", conv.Unparsed, "transaction.offer_price", "messages.2")

	serveJSON(f, "GET /api/v2/conversations/6000000001", `{"conversation":{"id":6000000001,"transaction":"none","messages":[]}}`)
	if conv, err = c.Conversation(ctx, 6000000001); err != nil {
		t.Fatal(err)
	}
	wantUnparsed(t, "conversation", conv.Unparsed, "transaction")

	serveJSON(f, "GET /api/v2/conversations/6000000001", `{"conversation":{"messages":[]}}`)
	if _, err := c.Conversation(ctx, 6000000001); kindOf(err) != KindAPIChanged {
		t.Fatalf("conversation without id err = %v", err)
	}
}

// A sent message stays sent when Vinted's echo of the conversation has a
// shape the client does not know.
func TestSendMessageOddEcho(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	serveJSON(f, "POST /api/v2/conversations/6000000001/replies", `{"conversation":"queued","reply":{"id":7000000009,"created_at_ts":"2026-10-17T12:05:00+02:00"}}`)
	msg, err := c.SendMessage(context.Background(), 6000000001, "Great")
	if err != nil || msg.ID != 7000000009 || !msg.FromSelf || msg.Text != "Great" || msg.At.Year() != 2026 {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
	wantUnparsed(t, "message", msg.Unparsed, "conversation")
}

func TestSavedSearchOddShapes(t *testing.T) {
	f, c := newFake(t)
	loggedIn(c)
	serveJSON(f, "GET /web/gateway/search-bar/v2/users/3000000001/previous_searches", `{"previous_searches":[
		{"id":"11","title":"levis","type":"type_saved","search_text":"levis","new_items_count":"4","price_from":"5.0","price_to":[30],
		 "selected_filters":[{"code":"brand","ids":["10",{"id":11},12.0]},"size:4",{"code":"status","ids":[2]}]},
		{"id":12,"title":"nike","type":"type_saved","selected_filters":{"brand":[53]}},
		false
	]}`)
	list, err := c.SavedSearches(context.Background(), false)
	if err != nil || len(list) != 3 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	s := list[0]
	if s.ID != 11 || !s.Saved || s.NewItems != 4 || s.Query.Text != "levis" || s.Query.PriceFrom != 5 || s.Query.PriceTo != 0 ||
		len(s.Query.BrandIDs) != 2 || s.Query.BrandIDs[0] != 10 || s.Query.BrandIDs[1] != 12 || len(s.Query.StatusIDs) != 1 || s.Query.StatusIDs[0] != 2 {
		t.Fatalf("saved search = %+v", s)
	}
	wantUnparsed(t, "saved search", s.Unparsed, "price_to", "selected_filters.0.ids.1", "selected_filters.1")
	if list[1].ID != 12 || list[1].Query.BrandIDs != nil {
		t.Fatalf("saved search 2 = %+v", list[1])
	}
	wantUnparsed(t, "saved search 2", list[1].Unparsed, "selected_filters")
	wantUnparsed(t, "unreadable saved search", list[2].Unparsed, "previous_searches.2")
}

// RFC 7519 allows a fractional NumericDate; the expiry must still schedule
// renewals.
func TestJWTExpiryAcceptsFractionalAndStringExp(t *testing.T) {
	for _, exp := range []string{`1700000000.5`, `"1700000000"`, `1700000000`} {
		tok := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":`+exp+`}`)) + ".c2ln"
		if got := jwtExpiry(tok); got.Unix() != 1700000000 {
			t.Errorf("exp %s: expiry = %v", exp, got)
		}
	}
}
