package vinted

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// defaultPerPage matches what the web app asks for on profile pages.
const defaultPerPage = 20

type rawUser struct {
	ID                 flexInt   `json:"id"`
	Login              string    `json:"login"`
	ProfileURL         string    `json:"profile_url"`
	CountryTitle       string    `json:"country_title"`
	CountryCode        string    `json:"country_code"`
	City               string    `json:"city"`
	ItemCount          int       `json:"item_count"`
	FeedbackCount      int       `json:"feedback_count"`
	FeedbackReputation flexFloat `json:"feedback_reputation"`
	FollowersCount     int       `json:"followers_count"`
	FollowingCount     int       `json:"following_count"`
	Business           bool      `json:"business"`
	LastLoggedOnTS     flexTime  `json:"last_loged_on_ts"`
	Verification       struct {
		Email struct {
			Valid bool `json:"valid"`
		} `json:"email"`
		// Phone is only present on /users/current.
		Phone *struct {
			Valid bool `json:"valid"`
		} `json:"phone"`
	} `json:"verification"`
}

func (c *Client) normalizeUser(u rawUser) User {
	out := User{
		ID:        int64(u.ID),
		Login:     u.Login,
		URL:       u.ProfileURL,
		Country:   u.CountryTitle,
		City:      u.City,
		Items:     u.ItemCount,
		Reviews:   u.FeedbackCount,
		Followers: u.FollowersCount,
		Following: u.FollowingCount,
		// feedback_reputation is 0-1; the profile shows it as stars.
		Rating:        float64(u.FeedbackReputation) * 5,
		Business:      u.Business,
		EmailVerified: u.Verification.Email.Valid,
		LastSeen:      u.LastLoggedOnTS.Time,
	}
	if p := u.Verification.Phone; p != nil {
		out.PhoneVerified = &p.Valid
	}
	if out.Country == "" {
		out.Country = u.CountryCode
	}
	if out.URL == "" && out.ID != 0 {
		out.URL = c.Base + "/member/" + strconv.FormatInt(out.ID, 10)
	}
	return out
}

// User fetches a public profile.
func (c *Client) User(ctx context.Context, id int64) (User, error) {
	if id <= 0 {
		return User{}, Usage("user id must be positive")
	}
	var out struct {
		User rawUser `json:"user"`
	}
	if err := c.getJSON(ctx, "/api/v2/users/"+strconv.FormatInt(id, 10), nil, false, &out); err != nil {
		return User{}, notFoundAs(err, "user %d does not exist", id)
	}
	return c.normalizeUser(out.User), nil
}

// Me fetches the logged-in account.
func (c *Client) Me(ctx context.Context) (User, error) {
	var out struct {
		User rawUser `json:"user"`
	}
	if err := c.getJSON(ctx, "/api/v2/users/current", nil, true, &out); err != nil {
		return User{}, err
	}
	return c.normalizeUser(out.User), nil
}

// rawAPIItem is the item shape of /api/v2 lists (wardrobe, favourites).
type rawAPIItem struct {
	ID             flexInt    `json:"id"`
	Title          string     `json:"title"`
	UserID         flexInt    `json:"user_id"`
	Price          *rawMoney  `json:"price"`
	Currency       string     `json:"currency"`
	TotalItemPrice *rawMoney  `json:"total_item_price"`
	URL            string     `json:"url"`
	Path           string     `json:"path"`
	IsClosed       bool       `json:"is_closed"`
	IsReserved     bool       `json:"is_reserved"`
	IsHidden       bool       `json:"is_hidden"`
	IsFavourite    bool       `json:"is_favourite"`
	Promoted       bool       `json:"promoted"`
	FavouriteCount int        `json:"favourite_count"`
	ViewCount      int        `json:"view_count"`
	Brand          string     `json:"brand"`
	BrandTitle     string     `json:"brand_title"`
	Size           string     `json:"size"`
	SizeTitle      string     `json:"size_title"`
	Status         string     `json:"status"`
	CatalogID      flexInt    `json:"catalog_id"`
	Photos         []rawPhoto `json:"photos"`
	Photo          *rawPhoto  `json:"photo"`
	User           *struct {
		ID    flexInt `json:"id"`
		Login string  `json:"login"`
	} `json:"user"`
}

func (c *Client) normalizeAPIItem(r rawAPIItem) Item {
	it := Item{
		ID:         int64(r.ID),
		Title:      r.Title,
		Price:      r.Price.amount(),
		Currency:   r.Price.currency(),
		TotalPrice: r.TotalItemPrice.amount(),
		Brand:      firstNonEmpty(r.Brand, r.BrandTitle),
		Size:       firstNonEmpty(r.Size, r.SizeTitle),
		Condition:  r.Status,
		CategoryID: int(r.CatalogID),
		SellerID:   int64(r.UserID),
		Favorites:  r.FavouriteCount,
		Views:      r.ViewCount,
		Reserved:   r.IsReserved,
		Sold:       r.IsClosed,
		Hidden:     r.IsHidden,
		Favorited:  r.IsFavourite,
		Promoted:   r.Promoted,
		URL:        r.URL,
	}
	if it.Currency == "" {
		it.Currency = r.Currency
	}
	photos := r.Photos
	if len(photos) == 0 && r.Photo != nil {
		photos = []rawPhoto{*r.Photo}
	}
	it.Images = photoURLs(photos)
	// The main photo's upload time is the closest thing to a listing date
	// these lists expose.
	it.CreatedAt = photoTime(photos)
	if r.User != nil {
		if it.SellerID == 0 {
			it.SellerID = int64(r.User.ID)
		}
		it.SellerLogin = r.User.Login
	}
	if it.URL == "" {
		it.URL = c.absURL(r.Path, it.ID)
	}
	return it
}

type rawItemList struct {
	Items      []rawAPIItem  `json:"items"`
	Pagination rawPagination `json:"pagination"`
}

func (c *Client) itemList(l rawItemList) SearchPage {
	out := SearchPage{Items: make([]Item, 0, len(l.Items)), Page: l.Pagination.CurrentPage, TotalPages: l.Pagination.TotalPages, Total: l.Pagination.TotalEntries}
	for _, r := range l.Items {
		out.Items = append(out.Items, c.normalizeAPIItem(r))
	}
	return out
}

func pageQuery(page, perPage int) url.Values {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	return url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}
}

// UserItems lists a member's wardrobe, newest first as on the profile.
func (c *Client) UserItems(ctx context.Context, id int64, page, perPage int) (SearchPage, error) {
	if id <= 0 {
		return SearchPage{}, Usage("user id must be positive")
	}
	q := pageQuery(page, perPage)
	q.Set("order", "relevance")
	var out rawItemList
	if err := c.getJSON(ctx, "/api/v2/wardrobe/"+strconv.FormatInt(id, 10)+"/items", q, false, &out); err != nil {
		return SearchPage{}, notFoundAs(err, "user %d does not exist", id)
	}
	return c.itemList(out), nil
}

// Favorites lists the logged-in account's favourited items.
func (c *Client) Favorites(ctx context.Context, page int) (SearchPage, error) {
	me, err := c.selfID()
	if err != nil {
		return SearchPage{}, err
	}
	var out rawItemList
	if err := c.getJSON(ctx, "/api/v2/users/"+strconv.FormatInt(me, 10)+"/items/favourites", pageQuery(page, defaultPerPage), true, &out); err != nil {
		return SearchPage{}, err
	}
	return c.itemList(out), nil
}

// ToggleFavorite flips the favourite state of an item, as the heart button
// does. Vinted only offers a toggle; read Item(...).Favorited first to know
// which way it will go.
func (c *Client) ToggleFavorite(ctx context.Context, itemID int64) error {
	if itemID <= 0 {
		return Usage("item id must be positive")
	}
	body := map[string]any{"type": "item", "user_favourites": []int64{itemID}}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/user_favourites/toggle", body: body, auth: true, csrf: true}, nil)
	return notFoundAs(err, "item %d does not exist or was removed", itemID)
}

// selfID is the logged-in account id from the session's token.
func (c *Client) selfID() (int64, error) {
	if c.Session == nil {
		return 0, &Error{Kind: KindAuth, Msg: "this command needs a logged-in profile. Run `vinted auth login`"}
	}
	id := c.Session.UserID()
	if id == 0 {
		return 0, &Error{Kind: KindAuth, Msg: "the stored cookies belong to a logged-out browser. Log in on vinted, export the cookies again and run `vinted auth login`"}
	}
	return id, nil
}

type rawFeedback struct {
	ID             flexInt  `json:"id"`
	Rating         int      `json:"rating"`
	Feedback       string   `json:"feedback"`
	CreatedAtTS    flexTime `json:"created_at_ts"`
	CreatedAt      flexTime `json:"created_at"`
	FeedbackUserID flexInt  `json:"feedback_user_id"`
	ItemTitle      string   `json:"item_title"`
	User           *struct {
		ID    flexInt `json:"id"`
		Login string  `json:"login"`
	} `json:"user"`
}

// Reviews lists the feedback a member received, newest first.
func (c *Client) Reviews(ctx context.Context, id int64, page int) ([]Review, error) {
	if id <= 0 {
		return nil, Usage("user id must be positive")
	}
	q := pageQuery(page, defaultPerPage)
	q.Set("user_id", strconv.FormatInt(id, 10))
	var out struct {
		UserFeedbacks []rawFeedback `json:"user_feedbacks"`
	}
	if err := c.getJSON(ctx, "/api/v2/user_feedbacks", q, false, &out); err != nil {
		return nil, notFoundAs(err, "user %d does not exist", id)
	}
	reviews := make([]Review, 0, len(out.UserFeedbacks))
	for _, f := range out.UserFeedbacks {
		r := Review{ID: int64(f.ID), Rating: f.Rating, Comment: f.Feedback, Date: f.CreatedAtTS.Time, ByID: int64(f.FeedbackUserID), ItemTitle: f.ItemTitle}
		if r.Date.IsZero() {
			r.Date = f.CreatedAt.Time
		}
		if f.User != nil {
			if r.ByID == 0 {
				r.ByID = int64(f.User.ID)
			}
			r.ByLogin = f.User.Login
		}
		reviews = append(reviews, r)
	}
	return reviews, nil
}

type rawCatalog struct {
	ID       int          `json:"id"`
	Title    string       `json:"title"`
	Catalogs []rawCatalog `json:"catalogs"`
}

func (r rawCatalog) category() Category {
	c := Category{ID: r.ID, Title: r.Title}
	for _, child := range r.Catalogs {
		c.Children = append(c.Children, child.category())
	}
	return c
}

// Categories returns the full category tree (the one the upload form uses,
// whose ids are the catalog[] search filter).
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	var out struct {
		Catalogs []rawCatalog `json:"catalogs"`
	}
	if err := c.getJSON(ctx, "/api/v2/item_upload/catalogs", nil, false, &out); err != nil {
		return nil, err
	}
	cats := make([]Category, 0, len(out.Catalogs))
	for _, r := range out.Catalogs {
		cats = append(cats, r.category())
	}
	return cats, nil
}

// Brands searches brands by name; the ids are the brand_ids[] search filter.
func (c *Client) Brands(ctx context.Context, keyword string) ([]Brand, error) {
	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	var out struct {
		Brands []struct {
			ID        int    `json:"id"`
			Title     string `json:"title"`
			Slug      string `json:"slug"`
			ItemCount int    `json:"item_count"`
		} `json:"brands"`
	}
	if err := c.getJSON(ctx, "/api/v2/brands", q, false, &out); err != nil {
		return nil, err
	}
	brands := make([]Brand, 0, len(out.Brands))
	for _, b := range out.Brands {
		brands = append(brands, Brand{ID: b.ID, Title: b.Title, Slug: b.Slug, Items: b.ItemCount})
	}
	return brands, nil
}

// notFoundAs replaces a generic KindNotFound with a human subject.
func notFoundAs(err error, format string, args ...any) error {
	var ve *Error
	if errors.As(err, &ve) && ve.Kind == KindNotFound {
		nf := NotFound(format, args...)
		nf.Status, nf.Endpoint, nf.Body = ve.Status, ve.Endpoint, ve.Body
		return nf
	}
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
