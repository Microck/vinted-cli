package vinted

import (
	"encoding/json"
	"net/url"
	"time"
)

// The types in this file are the CLI's stable output contract. Vinted's raw
// shapes (the escaped Next.js RSC payload of catalog and item pages, the
// /api/v2 JSON with its string prices and photo arrays) are normalized into
// these once, in this package.

type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// Item is a listing. Search results fill the summary fields; the item page
// (`item show`) fills everything it shows.
type Item struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Price       float64 `json:"price"`
	Currency    string  `json:"currency"`
	// TotalPrice includes Vinted's buyer protection fee, not shipping.
	TotalPrice         float64   `json:"total_price,omitempty"`
	BuyerProtectionFee float64   `json:"buyer_protection_fee,omitempty"`
	Shipping           *Shipping `json:"shipping,omitempty"`
	Brand              string    `json:"brand,omitempty"`
	BrandID            int       `json:"brand_id,omitempty"`
	Size               string    `json:"size,omitempty"`
	Condition          string    `json:"condition,omitempty"`
	// Color is the colour line as the page shows it ("Black, Gray").
	Color      string `json:"color,omitempty"`
	CategoryID int    `json:"category_id,omitempty"`
	// CategoryPath runs from the top category down to CategoryID.
	CategoryPath []CategoryRef `json:"category_path,omitempty"`
	// Attributes is the item page's details list as shown (brand, size,
	// condition, colour, material, measurements, upload date... whichever
	// the listing has), in page order.
	Attributes  []ItemAttribute `json:"attributes,omitempty"`
	SellerID    int64           `json:"seller_id,omitempty"`
	SellerLogin string          `json:"seller_login,omitempty"`
	Seller      *ItemSeller     `json:"seller,omitempty"`
	Favorites   int             `json:"favorites"`
	Views       int             `json:"views,omitempty"`
	Reserved    bool            `json:"reserved"`
	Sold        bool            `json:"sold"`
	Hidden      bool            `json:"hidden"`
	Favorited   bool            `json:"favorited"`
	// FavoritedKnown is set when the item page stated Favorited; when it is
	// false, Favorited is only the zero value and says nothing.
	FavoritedKnown bool `json:"-"`
	Promoted       bool `json:"promoted"`
	// CanBuy reports whether the buy button is offered to the viewer; only
	// the item page says.
	CanBuy *bool    `json:"can_buy,omitempty"`
	Images []string `json:"images,omitempty"`
	// Photos are Images with their size, from the item page.
	Photos []ItemPhoto `json:"photos,omitempty"`
	URL    string      `json:"url"`
	// Uploaded is the listing age as the page words it ("6 hours ago");
	// other people's items carry no exact date.
	Uploaded  string    `json:"uploaded,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	// Unparsed holds, by JSON path, the raw page data the cli has no reader
	// for or that came in a shape it does not know (plugins.<name>, ...),
	// so nothing on the page is lost.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

// ItemPhoto is one listing photo. URL is the largest rendition the page links
// (f800), which can be smaller than the uploaded original whose size is
// given: a 1200x1600 upload is served at 600x800.
type ItemPhoto struct {
	URL            string `json:"url"`
	OriginalWidth  int    `json:"original_width,omitempty"`
	OriginalHeight int    `json:"original_height,omitempty"`
	Main           bool   `json:"main,omitempty"`
	DominantColor  string `json:"dominant_color,omitempty"`
}

// Shipping is the postage line of the item page. Price is what the buyer
// pays for the cheapest option; with several options (FromPrice) it is the
// "from" price and the final one depends on the option picked at checkout.
type Shipping struct {
	Price     float64 `json:"price"`
	Currency  string  `json:"currency,omitempty"`
	Free      bool    `json:"free"`
	FromPrice bool    `json:"from_price"`
	// Delivery is the default delivery type ("home", "pickup").
	Delivery string `json:"delivery,omitempty"`
}

// CategoryRef is one step of an item's category path.
type CategoryRef struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// ItemAttribute is one line of the item page's details: Code is Vinted's
// attribute code (brand, size, status, color, upload_date, ...), Title the
// label shown and Value the value shown.
type ItemAttribute struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Value string `json:"value"`
}

// ItemSeller is the seller box of the item page. Profile is the seller's
// public profile (city, country, exact last login), loaded only on request
// since it is one more request (`item show --seller`).
type ItemSeller struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	// Rating is 0-5, as the stars on the page.
	Rating   float64 `json:"rating"`
	Reviews  int     `json:"reviews"`
	Business bool    `json:"business"`
	// LastSeen is the page's wording ("Last seen an hour ago").
	LastSeen string   `json:"last_seen,omitempty"`
	Badges   []string `json:"badges,omitempty"`
	// Info is every line of the seller box as shown, LastSeen included.
	Info    []string `json:"info,omitempty"`
	Profile *User    `json:"profile,omitempty"`
}

// SearchParams maps one-to-one onto the catalog page query. Attributes holds
// the filters without a field of their own, by filter code (see Filters):
// code "video_game_rating" becomes video_game_rating_ids[]. Extra carries any
// other catalog query parameter verbatim (`--filter k=v`).
type SearchParams struct {
	Text        string  `json:"text,omitempty"`
	CatalogIDs  []int   `json:"catalog_ids,omitempty"`
	BrandIDs    []int   `json:"brand_ids,omitempty"`
	SizeIDs     []int   `json:"size_ids,omitempty"`
	StatusIDs   []int   `json:"status_ids,omitempty"`
	ColorIDs    []int   `json:"color_ids,omitempty"`
	MaterialIDs []int   `json:"material_ids,omitempty"`
	PriceFrom   float64 `json:"price_from,omitempty"`
	PriceTo     float64 `json:"price_to,omitempty"`
	// Currency is the currency of PriceFrom/PriceTo (ISO code); empty is the
	// site's own.
	Currency   string           `json:"currency,omitempty"`
	Attributes map[string][]int `json:"attributes,omitempty"`
	// Order is relevance, newest_first, price_low_to_high or price_high_to_low.
	Order   string     `json:"order,omitempty"`
	Page    int        `json:"page,omitempty"`
	PerPage int        `json:"per_page,omitempty"`
	Extra   url.Values `json:"extra,omitempty"`
}

type SearchPage struct {
	Items      []Item `json:"items"`
	Page       int    `json:"page"`
	TotalPages int    `json:"total_pages"`
	Total      int    `json:"total"`
	// Unparsed holds, by JSON path, list entries or parts of the response in
	// a shape the cli does not know ("items.3", "pagination").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	URL       string `json:"url,omitempty"`
	Country   string `json:"country,omitempty"`
	City      string `json:"city,omitempty"`
	Items     int    `json:"items"`
	Reviews   int    `json:"reviews"`
	Followers int    `json:"followers"`
	Following int    `json:"following"`
	// Rating is 0-5.
	Rating   float64 `json:"rating"`
	Business bool    `json:"business"`
	// EmailVerified is the "email verified" badge on the profile.
	EmailVerified bool `json:"email_verified"`
	// PhoneVerified is only known for the logged-in account's own profile;
	// Vinted asks for it (code 115) before the inbox and favourites work.
	PhoneVerified *bool     `json:"phone_verified,omitempty"`
	LastSeen      time.Time `json:"last_seen,omitzero"`
	// Unparsed holds, by JSON path, profile parts in a shape the cli does
	// not know ("verification.phone").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type Review struct {
	ID        int64     `json:"id"`
	Rating    int       `json:"rating"`
	Comment   string    `json:"comment,omitempty"`
	Date      time.Time `json:"date"`
	ByID      int64     `json:"by_id,omitempty"`
	ByLogin   string    `json:"by_login,omitempty"`
	ItemTitle string    `json:"item_title,omitempty"`
	// Unparsed holds, by JSON path, review parts in a shape the cli does not
	// know ("user", or "user_feedbacks.N" for a whole unreadable review).
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type Category struct {
	ID       int        `json:"id"`
	Title    string     `json:"title"`
	Children []Category `json:"children,omitempty"`
	// Unparsed holds, by JSON path, subcategories in a shape the cli does
	// not know ("catalogs.N").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type Brand struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Slug  string `json:"slug,omitempty"`
	Items int    `json:"items,omitempty"`
	// Unparsed holds a brand entry in a shape the cli does not know
	// ("brands.N").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type Conversation struct {
	ID          int64     `json:"id"`
	WithUser    ChatUser  `json:"with_user"`
	Item        ChatItem  `json:"item,omitzero"`
	Unread      bool      `json:"unread"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	LastMessage string    `json:"last_message,omitempty"`
	Messages    []Message `json:"messages,omitempty"`
	// Unparsed holds, by JSON path, conversation parts in a shape the cli
	// does not know ("opposite_user", "transaction", "messages.N"), or a
	// whole inbox row ("conversations.N") when it cannot be read at all.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type ChatUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type ChatItem struct {
	ID       int64   `json:"id,omitempty"`
	Title    string  `json:"title,omitempty"`
	Price    float64 `json:"price,omitempty"`
	Currency string  `json:"currency,omitempty"`
}

type Message struct {
	ID       int64     `json:"id"`
	FromSelf bool      `json:"from_self"`
	Text     string    `json:"text"`
	At       time.Time `json:"at"`
	// Unparsed holds, by JSON path, message parts in a shape the cli does
	// not know ("entity").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}
