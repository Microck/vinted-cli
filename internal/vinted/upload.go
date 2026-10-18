package vinted

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// Listing management mirrors the web app's upload form (/items/new): photos
// go to /api/v2/photos first, then one JSON call creates, saves as draft or
// updates the listing under /api/v2/item_upload. Visibility and deletion are
// separate /api/v2/items/{id} calls. Every write answers code 115
// (user_verification_required) until Vinted has verified the account.

// UploadImage is a photo file to attach to a listing.
type UploadImage struct {
	Name        string
	Data        []byte
	ContentType string
}

// Photo is a photo uploaded for a listing, not yet attached to one.
type Photo struct {
	ID       int64  `json:"id"`
	URL      string `json:"url"`
	TempUUID string `json:"temp_uuid"`
	// Unparsed holds, by JSON path, answer parts in a shape the cli does
	// not know.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

// UploadAttribute is one category-specific field of the upload form: size,
// condition, material, ... Options are the selectable values, flattened.
type UploadAttribute struct {
	Code     string            `json:"code"`
	Title    string            `json:"title"`
	Required bool              `json:"required"`
	Multi    bool              `json:"multi"`
	Limit    int               `json:"limit,omitempty"`
	Options  []AttributeOption `json:"options"`
	// Unparsed holds, by JSON path, parts of the attribute in a shape the
	// cli does not know ("configuration.options.2"). A whole attribute it
	// could not read appears without a code, under "attributes.N".
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

// AttributeOption is one selectable value (an id the API takes, a title users read).
type AttributeOption struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// AttributeValue is a selection sent in item_attributes.
type AttributeValue struct {
	Code string `json:"code"`
	IDs  []int  `json:"ids"`
}

// PackageSize is a shipping size the seller picks for a category.
type PackageSize struct {
	ID    int    `json:"id"`
	Code  string `json:"code"`
	Title string `json:"title"`
	Name  string `json:"name"`
	// Unparsed holds, by JSON path, parts of the size in a shape the cli
	// does not know.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

// ItemInput is everything the upload form submits for a new Item.
type ItemInput struct {
	Title         string
	Description   string
	Price         float64
	Currency      string
	CatalogID     int
	BrandID       int
	Brand         string
	ColorIDs      []int
	PackageSizeID int
	Attributes    []AttributeValue
	Photos        []UploadImage
}

// ItemEdit changes one of the account's Items. Nil fields keep their value;
// Attributes replace only the codes they name; Photos, when set, replace the
// whole gallery.
type ItemEdit struct {
	Title         *string
	Description   *string
	Price         *float64
	CatalogID     *int
	BrandID       *int
	Brand         *string
	ColorIDs      []int
	PackageSizeID *int
	Attributes    []AttributeValue
	Photos        []UploadImage
}

// UploadedItem is the account's own Item as the upload API returns it.
type UploadedItem struct {
	Item
	Draft bool `json:"draft"`
}

// newUUID returns a random RFC 4122 version 4 id, the form the web app uses
// for temp_uuid and upload_session_id.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// UploadPhoto sends one image to /api/v2/photos. tempUUID groups the photos
// of one upload form; the listing then references them by id.
func (c *Client) UploadPhoto(ctx context.Context, img UploadImage, tempUUID string) (Photo, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("photo[type]", "item")
	if tempUUID != "" {
		_ = mw.WriteField("photo[temp_uuid]", tempUUID)
	}
	h := textproto.MIMEHeader{}
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(img.Name)
	h.Set("Content-Disposition", `form-data; name="photo[file]"; filename="`+name+`"`)
	ct := img.ContentType
	if ct == "" {
		ct = http.DetectContentType(img.Data)
	}
	h.Set("Content-Type", ct)
	part, err := mw.CreatePart(h)
	if err != nil {
		return Photo{}, err
	}
	if _, err := part.Write(img.Data); err != nil {
		return Photo{}, err
	}
	if err := mw.Close(); err != nil {
		return Photo{}, err
	}
	// The photo is stored once this answers, so only its id may fail it.
	var out struct {
		ID       flexInt         `json:"id"`
		URL      lenient[string] `json:"url"`
		TempUUID lenient[string] `json:"temp_uuid"`
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/photos", raw: buf.Bytes(), contentType: mw.FormDataContentType(), auth: true, csrf: true}, &out); err != nil {
		return Photo{}, err
	}
	if out.ID == 0 {
		return Photo{}, &Error{Kind: KindAPIChanged, Endpoint: "POST /api/v2/photos", Msg: "vinted accepted the photo but returned no photo id"}
	}
	p := Photo{ID: int64(out.ID), URL: out.URL.V, TempUUID: out.TempUUID.V}
	keepRaw(&p.Unparsed, "url", out.URL.Raw)
	keepRaw(&p.Unparsed, "temp_uuid", out.TempUUID.Raw)
	return p, nil
}

// rawAttributeOptions keeps each option lenient on its own, so one option of
// another shape does not cost the attribute its other options.
type rawAttributeOptions []lenient[rawAttributeOption]

// rawAttributeOption is one option or option group; its id and title are core.
type rawAttributeOption struct {
	ID      flexInt                      `json:"id"`
	Title   string                       `json:"title"`
	Type    lenient[string]              `json:"type"`
	Options lenient[rawAttributeOptions] `json:"options"`
}

// flattenOptions collects the selectable leaves under option groups found
// at path, recording in *unparsed the parts it could not read.
func flattenOptions(list lenient[rawAttributeOptions], out []AttributeOption, unparsed *map[string]json.RawMessage, path string) []AttributeOption {
	keepRaw(unparsed, path, list.Raw)
	for i, lo := range list.V {
		at := path + "." + strconv.Itoa(i)
		if lo.Raw != nil {
			keepRaw(unparsed, at, lo.Raw)
			continue
		}
		o := lo.V
		keepRaw(unparsed, at+".type", o.Type.Raw)
		if len(o.Options.V) > 0 || o.Options.Raw != nil || o.Type.V == "group" {
			out = flattenOptions(o.Options, out, unparsed, at+".options")
			continue
		}
		out = append(out, AttributeOption{ID: int(o.ID), Title: o.Title})
	}
	return out
}

// rawUploadAttribute is one field of the upload form. Its code is core; the
// configuration is lenient part by part.
type rawUploadAttribute struct {
	Code          string                       `json:"code"`
	Configuration lenient[*rawAttributeConfig] `json:"configuration"`
}

type rawAttributeConfig struct {
	Title          lenient[string]              `json:"title"`
	Required       lenient[bool]                `json:"required"`
	SelectionType  lenient[string]              `json:"selection_type"`
	SelectionLimit flexInt                      `json:"selection_limit"`
	Options        lenient[rawAttributeOptions] `json:"options"`
}

// UploadAttributes lists the category-specific fields of the upload form
// (size, condition, material, ...) with their options. Brand and colour have
// their own endpoints and are not included.
func (c *Client) UploadAttributes(ctx context.Context, catalogID int) ([]UploadAttribute, error) {
	if catalogID <= 0 {
		return nil, Usage("category id must be positive")
	}
	body := map[string]any{"attributes": []map[string]any{{"code": "category", "value": []int{catalogID}}}}
	var out struct {
		Attributes []lenient[rawUploadAttribute] `json:"attributes"`
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/item_upload/attributes", body: body, auth: true, csrf: true}, &out); err != nil {
		return nil, err
	}
	attrs := make([]UploadAttribute, 0, len(out.Attributes))
	for i, la := range out.Attributes {
		if la.Raw != nil {
			a := UploadAttribute{Options: []AttributeOption{}}
			keepRaw(&a.Unparsed, "attributes."+strconv.Itoa(i), la.Raw)
			attrs = append(attrs, a)
			continue
		}
		ra := la.V
		if ra.Code == "" || (ra.Configuration.V == nil && ra.Configuration.Raw == nil) {
			continue
		}
		a := UploadAttribute{Code: ra.Code, Options: []AttributeOption{}}
		keepRaw(&a.Unparsed, "configuration", ra.Configuration.Raw)
		if cfg := ra.Configuration.V; cfg != nil {
			a.Title, a.Required = cfg.Title.V, cfg.Required.V
			a.Multi, a.Limit = cfg.SelectionType.V == "multi", int(cfg.SelectionLimit)
			a.Options = flattenOptions(cfg.Options, a.Options, &a.Unparsed, "configuration.options")
			keepRaw(&a.Unparsed, "configuration.title", cfg.Title.Raw)
			keepRaw(&a.Unparsed, "configuration.required", cfg.Required.Raw)
			keepRaw(&a.Unparsed, "configuration.selection_type", cfg.SelectionType.Raw)
		}
		attrs = append(attrs, a)
	}
	return attrs, nil
}

// UploadColors lists the colours a listing can carry (at most two per listing).
func (c *Client) UploadColors(ctx context.Context) ([]AttributeOption, error) {
	var out struct {
		Colors []struct {
			ID    flexInt `json:"id"`
			Title string  `json:"title"`
		} `json:"colors"`
	}
	if err := c.getJSON(ctx, "/api/v2/item_upload/colors", nil, true, &out); err != nil {
		return nil, err
	}
	colors := make([]AttributeOption, 0, len(out.Colors))
	for _, col := range out.Colors {
		colors = append(colors, AttributeOption{ID: int(col.ID), Title: col.Title})
	}
	return colors, nil
}

// UploadBrands searches the brands the upload form offers for a category.
func (c *Client) UploadBrands(ctx context.Context, keyword string, catalogID int) ([]Brand, error) {
	q := url.Values{"keyword": {keyword}}
	if catalogID > 0 {
		q.Set("category_id", strconv.Itoa(catalogID))
	}
	var out struct {
		Brands []struct {
			ID    flexInt `json:"id"`
			Title string  `json:"title"`
		} `json:"brands"`
	}
	if err := c.getJSON(ctx, "/api/v2/item_upload/brands", q, true, &out); err != nil {
		return nil, err
	}
	brands := make([]Brand, 0, len(out.Brands))
	for _, b := range out.Brands {
		brands = append(brands, Brand{ID: int(b.ID), Title: b.Title})
	}
	return brands, nil
}

// PackageSizes lists the shipping sizes offered for a category, smallest first.
func (c *Client) PackageSizes(ctx context.Context, catalogID int) ([]PackageSize, error) {
	if catalogID <= 0 {
		return nil, Usage("category id must be positive")
	}
	var out struct {
		PackageSizes []struct {
			ID    flexInt         `json:"id"`
			Code  string          `json:"code"`
			Title lenient[string] `json:"title"`
			Name  lenient[string] `json:"name"`
		} `json:"package_sizes"`
	}
	path := "/shipping-estimation/external/catalogs/" + strconv.Itoa(catalogID) + "/package_sizes"
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path, auth: true, gateway: true}, &out); err != nil {
		return nil, err
	}
	sizes := make([]PackageSize, 0, len(out.PackageSizes))
	for _, p := range out.PackageSizes {
		s := PackageSize{ID: int(p.ID), Code: p.Code, Title: p.Title.V, Name: p.Name.V}
		keepRaw(&s.Unparsed, "title", p.Title.Raw)
		keepRaw(&s.Unparsed, "name", p.Name.Raw)
		sizes = append(sizes, s)
	}
	return sizes, nil
}

// uploadPhotos uploads every image under one temp uuid and returns the
// assigned_photos entries the item payload references them by.
func (c *Client) uploadPhotos(ctx context.Context, imgs []UploadImage, tempUUID string) ([]map[string]any, error) {
	assigned := make([]map[string]any, 0, len(imgs))
	for i, img := range imgs {
		p, err := c.UploadPhoto(ctx, img, tempUUID)
		if err != nil {
			return nil, fmt.Errorf("upload photo %d (%s): %w", i+1, img.Name, err)
		}
		assigned = append(assigned, map[string]any{"id": p.ID, "orientation": 0})
	}
	return assigned, nil
}

// itemForm builds the item object the upload form submits.
func itemForm(in ItemInput, tempUUID string, photos []map[string]any) map[string]any {
	attrs := in.Attributes
	if attrs == nil {
		attrs = []AttributeValue{}
	}
	colors := in.ColorIDs
	if colors == nil {
		colors = []int{}
	}
	return map[string]any{
		"id":                     nil,
		"currency":               in.Currency,
		"temp_uuid":              tempUUID,
		"title":                  in.Title,
		"description":            in.Description,
		"brand_id":               nullInt(in.BrandID),
		"brand":                  in.Brand,
		"catalog_id":             in.CatalogID,
		"isbn":                   nil,
		"is_unisex":              false,
		"price":                  in.Price,
		"package_size_id":        nullInt(in.PackageSizeID),
		"shipment_prices":        map[string]any{"domestic": nil, "international": nil},
		"color_ids":              colors,
		"assigned_photos":        photos,
		"measurement_length":     nil,
		"measurement_width":      nil,
		"item_attributes":        attrs,
		"manufacturer":           nil,
		"manufacturer_labelling": nil,
	}
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// rawUploadedItem is the item or draft the upload endpoints answer with.
type rawUploadedItem struct {
	rawAPIItem
	IsDraft lenient[bool] `json:"is_draft"`
}

// uploadAnswer is the body of the create, draft and edit calls. Both parts
// are lenient: the write has already happened when it arrives, so an odd
// shape must not turn it into an error.
type uploadAnswer struct {
	Item  lenient[*rawUploadedItem] `json:"item"`
	Draft lenient[*rawUploadedItem] `json:"draft"`
}

func (c *Client) uploadedItem(endpoint string, ans uploadAnswer) (UploadedItem, error) {
	key, part, draft := "item", ans.Item, false
	if part.V == nil && part.Raw == nil {
		key, part, draft = "draft", ans.Draft, ans.Draft.V != nil || ans.Draft.Raw != nil
	}
	if part.Raw != nil {
		// Kept whole, with the id when it can still be read.
		var core struct {
			ID flexInt `json:"id"`
		}
		_ = json.Unmarshal(part.Raw, &core)
		if core.ID == 0 {
			return UploadedItem{}, &Error{Kind: KindAPIChanged, Endpoint: endpoint, Body: c.snippet(part.Raw), Msg: "vinted accepted the item but returned it in a shape the cli cannot read"}
		}
		up := UploadedItem{Item: Item{ID: int64(core.ID), URL: c.absURL("", int64(core.ID))}, Draft: draft}
		keepRaw(&up.Unparsed, key, part.Raw)
		return up, nil
	}
	r := part.V
	if r == nil || r.ID == 0 {
		return UploadedItem{}, &Error{Kind: KindAPIChanged, Endpoint: endpoint, Msg: "vinted accepted the item but did not return it"}
	}
	up := UploadedItem{Item: c.normalizeAPIItem(r.rawAPIItem), Draft: draft || r.IsDraft.V}
	keepRaw(&up.Unparsed, "is_draft", r.IsDraft.Raw)
	return up, nil
}

// CreateItem uploads the photos and publishes the listing, or saves it as
// a draft visible only to the account (finish it in the web app).
func (c *Client) CreateItem(ctx context.Context, in ItemInput, draft bool) (UploadedItem, error) {
	if len(in.Photos) == 0 {
		return UploadedItem{}, Usage("an item needs at least one photo")
	}
	tempUUID := newUUID()
	photos, err := c.uploadPhotos(ctx, in.Photos, tempUUID)
	if err != nil {
		return UploadedItem{}, err
	}
	item := itemForm(in, tempUUID, photos)
	var out uploadAnswer
	r := request{method: http.MethodPost, auth: true, csrf: true}
	if draft {
		r.path = "/api/v2/item_upload/drafts"
		r.body = map[string]any{"draft": item, "parcel": nil, "upload_session_id": newUUID()}
	} else {
		r.path = "/api/v2/item_upload/items"
		r.body = map[string]any{"item": item, "push_up": false, "parcel": nil, "upload_session_id": newUUID()}
		r.header = http.Header{"X-Upload-Form": {"true"}}
	}
	if _, err := c.do(ctx, r, &out); err != nil {
		return UploadedItem{}, err
	}
	up, err := c.uploadedItem(r.method+" "+r.path, out)
	if err != nil {
		return up, err
	}
	up.fillSubmitted(in.Title, in.Price, in.Currency)
	return up, nil
}

// fillSubmitted reports the submitted values for fields an upload answer
// leaves out: the drafts and edit endpoints return little more than the id.
func (up *UploadedItem) fillSubmitted(title string, price float64, currency string) {
	if up.Title == "" {
		up.Title = title
	}
	if up.Price == 0 {
		up.Price = price
	}
	if up.Currency == "" {
		up.Currency = currency
	}
}

// itemFormKeys are the fields the upload form PUTs back on edit.
var itemFormKeys = []string{
	"id", "currency", "temp_uuid", "title", "description", "brand_id", "brand", "catalog_id",
	"isbn", "is_unisex", "price", "package_size_id", "shipment_prices", "color_ids",
	"assigned_photos", "measurement_length", "measurement_width", "item_attributes",
	"manufacturer", "manufacturer_labelling", "model",
}

// editForm turns the stored listing into the upload form payload, keeping
// every value the edit does not touch.
func editForm(stored map[string]any) map[string]any {
	form := map[string]any{}
	for _, k := range itemFormKeys {
		if v, ok := stored[k]; ok {
			form[k] = v
		}
	}
	// The stored listing may carry the display shapes instead of the form
	// ones: a price object, a photos array.
	if p, ok := form["price"].(map[string]any); ok {
		form["price"] = p["amount"]
		if form["currency"] == nil {
			form["currency"] = p["currency_code"]
		}
	}
	if _, ok := form["assigned_photos"]; !ok {
		var assigned []map[string]any
		if photos, ok := stored["photos"].([]any); ok {
			for _, ph := range photos {
				if m, ok := ph.(map[string]any); ok && m["id"] != nil {
					assigned = append(assigned, map[string]any{"id": m["id"], "orientation": 0})
				}
			}
		}
		form["assigned_photos"] = assigned
	}
	if _, ok := form["brand"]; !ok {
		form["brand"] = stored["brand_title"]
	}
	if ids, ok := form["color_ids"].([]any); !ok || len(ids) == 0 {
		form["color_ids"] = storedColorIDs(stored)
	}
	return form
}

// storedColorIDs reads a listing's colours from whichever shape the stored
// item uses: color_ids, a colors list of objects, or color1_id/color2_id.
// Publishing requires a colour for most categories, so dropping it would make
// every edit fail with "Fill in color".
func storedColorIDs(stored map[string]any) []any {
	ids := []any{}
	if list, ok := stored["colors"].([]any); ok {
		for _, c := range list {
			if m, ok := c.(map[string]any); ok && m["id"] != nil {
				ids = append(ids, m["id"])
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}
	for _, k := range []string{"color1_id", "color2_id"} {
		if v, ok := stored[k]; ok && v != nil && fmt.Sprint(v) != "0" {
			ids = append(ids, v)
		}
	}
	return ids
}

// mergeAttributes replaces the codes named in edits and keeps the rest.
func mergeAttributes(current any, edits []AttributeValue) []any {
	replaced := map[string]bool{}
	for _, e := range edits {
		replaced[e.Code] = true
	}
	var out []any
	if list, ok := current.([]any); ok {
		for _, a := range list {
			if m, ok := a.(map[string]any); ok && replaced[fmt.Sprint(m["code"])] {
				continue
			}
			out = append(out, a)
		}
	}
	for _, e := range edits {
		out = append(out, map[string]any{"code": e.Code, "ids": e.IDs})
	}
	return out
}

// EditItem changes fields on one of the account's listings, as saving the
// web app's edit form does: the stored form is read, edited and sent back.
func (c *Client) EditItem(ctx context.Context, id int64, e ItemEdit) (UploadedItem, error) {
	if id <= 0 {
		return UploadedItem{}, Usage("item id must be positive")
	}
	path := "/api/v2/item_upload/items/" + strconv.FormatInt(id, 10)
	var stored struct {
		Item map[string]any `json:"item"`
	}
	if err := c.getJSON(ctx, path, nil, true, &stored); err != nil {
		return UploadedItem{}, notFoundAs(err, "item %d is not one of your items", id)
	}
	if stored.Item == nil {
		return UploadedItem{}, &Error{Kind: KindAPIChanged, Endpoint: "GET " + path, Msg: "vinted returned no editable item"}
	}
	form := editForm(stored.Item)
	form["id"] = id
	if e.Title != nil {
		form["title"] = *e.Title
	}
	if e.Description != nil {
		form["description"] = *e.Description
	}
	if e.Price != nil {
		form["price"] = *e.Price
	}
	if e.CatalogID != nil {
		form["catalog_id"] = *e.CatalogID
	}
	if e.BrandID != nil {
		form["brand_id"] = nullInt(*e.BrandID)
	}
	if e.Brand != nil {
		form["brand"] = *e.Brand
	}
	if e.ColorIDs != nil {
		form["color_ids"] = e.ColorIDs
	}
	if e.PackageSizeID != nil {
		form["package_size_id"] = *e.PackageSizeID
	}
	if len(e.Attributes) > 0 {
		form["item_attributes"] = mergeAttributes(form["item_attributes"], e.Attributes)
	}
	if len(e.Photos) > 0 {
		tempUUID := newUUID()
		photos, err := c.uploadPhotos(ctx, e.Photos, tempUUID)
		if err != nil {
			return UploadedItem{}, err
		}
		form["temp_uuid"] = tempUUID
		form["assigned_photos"] = photos
	}
	var out uploadAnswer
	body := map[string]any{"item": form, "push_up": false, "parcel": nil, "upload_session_id": newUUID()}
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, body: body, auth: true, csrf: true}, &out); err != nil {
		return UploadedItem{}, notFoundAs(err, "item %d is not one of your items", id)
	}
	up, err := c.uploadedItem("PUT "+path, out)
	if err != nil {
		return up, err
	}
	title, _ := form["title"].(string)
	currency, _ := form["currency"].(string)
	var price flexFloat
	_ = price.UnmarshalJSON([]byte(fmt.Sprint(form["price"])))
	up.fillSubmitted(title, float64(price), currency)
	// Editing does not change visibility; the answer just leaves it out.
	if hidden, _ := stored.Item["is_hidden"].(bool); hidden {
		up.Hidden = true
	}
	return up, nil
}

// DeleteItem removes one of the account's listings for good.
func (c *Client) DeleteItem(ctx context.Context, id int64) error {
	if id <= 0 {
		return Usage("item id must be positive")
	}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/items/" + strconv.FormatInt(id, 10) + "/delete", auth: true, csrf: true}, nil)
	return notFoundAs(err, "item %d is not one of your items", id)
}

// DeleteDraft discards one of the account's unpublished drafts.
func (c *Client) DeleteDraft(ctx context.Context, id int64) error {
	if id <= 0 {
		return Usage("draft id must be positive")
	}
	_, err := c.do(ctx, request{method: http.MethodDelete, path: "/api/v2/item_upload/drafts/" + strconv.FormatInt(id, 10), auth: true, csrf: true}, nil)
	return notFoundAs(err, "draft %d does not exist", id)
}

// SetItemHidden hides a listing from the catalog and profile, or shows it again.
func (c *Client) SetItemHidden(ctx context.Context, id int64, hidden bool) error {
	if id <= 0 {
		return Usage("item id must be positive")
	}
	body := map[string]any{"is_hidden": hidden}
	_, err := c.do(ctx, request{method: http.MethodPut, path: "/api/v2/items/" + strconv.FormatInt(id, 10) + "/is_hidden", body: body, auth: true, csrf: true}, nil)
	return notFoundAs(err, "item %d is not one of your items", id)
}

// AccountCurrency is the currency the account lists in (USD on vinted.com,
// EUR on most European sites).
func (c *Client) AccountCurrency(ctx context.Context) (string, error) {
	var out struct {
		User struct {
			Currency string `json:"currency"`
		} `json:"user"`
	}
	if err := c.getJSON(ctx, "/api/v2/users/current", nil, true, &out); err != nil {
		return "", err
	}
	return out.User.Currency, nil
}
