package vinted

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Offers and reservations hang off the transaction Vinted opens for every
// conversation about an item, as the conversation screen's buttons do:
// buyers send offer requests, sellers counter with offers, accept or reject
// requests, and reserve the item for that buyer.

// Offer is a price proposal in a conversation.
type Offer struct {
	ID           int64   `json:"id,omitempty"`
	Conversation int64   `json:"conversation"`
	Transaction  int64   `json:"transaction"`
	Amount       float64 `json:"amount"`
	Currency     string  `json:"currency"`
	Status       string  `json:"status"`
	// Kind is "request" (a buyer's offer) or "offer" (a seller's counter).
	Kind string `json:"kind"`
	// Unparsed holds, by JSON path, parts of the conversation or of the
	// answer in a shape the cli does not know
	// ("conversation.messages.4.entity").
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

// offerStatus maps the numeric offer request states the web app checks.
func offerStatus(raw []byte) string {
	s := strings.Trim(string(bytes.TrimSpace(raw)), `"`)
	switch s {
	case "10", "pending":
		return "pending"
	case "20", "accepted":
		return "accepted"
	case "30", "rejected":
		return "rejected"
	case "40", "cancelled":
		return "cancelled"
	}
	return s
}

// rawDeal is the part of a conversation the offer calls read. Only the
// transaction id is core; messages are read one by one and only the offer
// request entities are decoded, so other message kinds cannot fail it.
type rawDeal struct {
	Conversation struct {
		Transaction lenient[*rawDealTransaction]       `json:"transaction"`
		Messages    lenient[[]lenient[rawDealMessage]] `json:"messages"`
	} `json:"conversation"`
}

type rawDealTransaction struct {
	ID       flexInt            `json:"id"`
	UserSide lenient[string]    `json:"user_side"`
	SellerID flexInt            `json:"seller_id"`
	Offer    lenient[*rawMoney] `json:"offer_price"`
}

type rawDealMessage struct {
	EntityType string          `json:"entity_type"`
	Entity     json.RawMessage `json:"entity"`
}

type rawOfferRequest struct {
	OfferRequestID flexInt         `json:"offer_request_id"`
	UserID         flexInt         `json:"user_id"`
	Status         json.RawMessage `json:"status"`
	Price          flexMoneyAmount `json:"price"`
	Currency       lenient[string] `json:"currency"`
}

// flexMoneyAmount reads a price that is either "12.0" or {"amount":"12.0",...}.
// Raw keeps a value of any other shape, for an output's Unparsed.
type flexMoneyAmount struct {
	Amount   float64
	Currency string
	Raw      json.RawMessage
}

func (m *flexMoneyAmount) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var r rawMoney
		if err := json.Unmarshal(b, &r); err != nil {
			m.Raw = append(json.RawMessage(nil), b...)
			return nil
		}
		m.Amount, m.Currency = r.amount(), r.currency()
		return nil
	}
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
	if err != nil {
		m.Raw = append(json.RawMessage(nil), b...)
		return nil
	}
	m.Amount = v
	return nil
}

// deal is what the offer and reservation calls need from a conversation.
type deal struct {
	conv        int64
	transaction int64
	seller      bool
	currency    string
	// pending is the latest offer request awaiting the seller.
	pending *Offer
	// unparsed holds conversation parts in a shape the cli does not know,
	// for the Offer the call returns.
	unparsed map[string]json.RawMessage
}

func (c *Client) deal(ctx context.Context, convID int64) (deal, error) {
	if convID <= 0 {
		return deal{}, Usage("conversation id must be positive")
	}
	me, err := c.selfID()
	if err != nil {
		return deal{}, err
	}
	path := "/api/v2/conversations/" + strconv.FormatInt(convID, 10)
	var raw rawDeal
	if err := c.getJSON(ctx, path, nil, true, &raw); err != nil {
		return deal{}, notFoundAs(err, "conversation %d does not exist", convID)
	}
	if tr := raw.Conversation.Transaction.Raw; tr != nil {
		return deal{}, &Error{Kind: KindAPIChanged, Endpoint: "GET " + path, Body: c.snippet(tr), Msg: "vinted sent the conversation's transaction in a shape the cli cannot read"}
	}
	t := raw.Conversation.Transaction.V
	if t == nil || t.ID == 0 {
		return deal{}, Usage("conversation %d is not about an item, so it has no offers or reservation", convID)
	}
	d := deal{conv: convID, transaction: int64(t.ID), currency: t.Offer.V.currency()}
	d.seller = t.UserSide.V == "seller" || (t.SellerID != 0 && int64(t.SellerID) == me)
	keepRaw(&d.unparsed, "conversation.transaction.user_side", t.UserSide.Raw)
	keepRaw(&d.unparsed, "conversation.transaction.offer_price", t.Offer.Raw)
	keepRaw(&d.unparsed, "conversation.messages", raw.Conversation.Messages.Raw)
	for i, lm := range raw.Conversation.Messages.V {
		at := "conversation.messages." + strconv.Itoa(i)
		if lm.Raw != nil {
			keepRaw(&d.unparsed, at, lm.Raw)
			continue
		}
		if lm.V.EntityType != "offer_request_message" {
			continue
		}
		var e rawOfferRequest
		if err := json.Unmarshal(lm.V.Entity, &e); err != nil {
			keepRaw(&d.unparsed, at+".entity", lm.V.Entity)
			continue
		}
		keepRaw(&d.unparsed, at+".entity.price", e.Price.Raw)
		keepRaw(&d.unparsed, at+".entity.currency", e.Currency.Raw)
		currency := firstNonEmpty(e.Currency.V, e.Price.Currency)
		if d.currency == "" {
			d.currency = currency
		}
		if int64(e.UserID) == me || e.OfferRequestID == 0 {
			continue
		}
		o := &Offer{ID: int64(e.OfferRequestID), Conversation: convID, Transaction: d.transaction, Amount: e.Price.Amount, Currency: currency, Status: offerStatus(e.Status), Kind: "request"}
		if o.Status == "pending" {
			d.pending = o
		}
	}
	return d, nil
}

func formatAmount(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// SendOffer proposes a price: an offer request when the account is the
// buyer, a counter offer when it is the seller.
func (c *Client) SendOffer(ctx context.Context, convID int64, amount float64, currency string) (Offer, error) {
	if amount <= 0 {
		return Offer{}, Usage("offer amount must be positive")
	}
	d, err := c.deal(ctx, convID)
	if err != nil {
		return Offer{}, err
	}
	if currency == "" {
		currency = d.currency
	}
	if currency == "" {
		return Offer{}, Usage("could not tell the conversation's currency; pass --currency")
	}
	base := "/api/v2/transactions/" + strconv.FormatInt(d.transaction, 10)
	o := Offer{Conversation: convID, Transaction: d.transaction, Amount: amount, Currency: currency, Status: "pending", Unparsed: d.unparsed}
	// The offer is sent once this answers, so its shape must not fail it.
	type created struct {
		ID flexInt `json:"id"`
	}
	var out struct {
		OfferRequest lenient[*created] `json:"offer_request"`
		Offer        lenient[*created] `json:"offer"`
	}
	r := request{method: http.MethodPost, auth: true, csrf: true}
	if d.seller {
		o.Kind = "offer"
		r.path = base + "/offers"
		r.body = map[string]any{"offer": map[string]any{"price": formatAmount(amount), "currency": currency}}
	} else {
		o.Kind = "request"
		r.path = base + "/offer_requests"
		r.body = map[string]any{"offer_request": map[string]any{"price": formatAmount(amount), "currency": currency}}
	}
	if _, err := c.do(ctx, r, &out); err != nil {
		return Offer{}, err
	}
	switch {
	case out.OfferRequest.V != nil:
		o.ID = int64(out.OfferRequest.V.ID)
	case out.Offer.V != nil:
		o.ID = int64(out.Offer.V.ID)
	}
	keepRaw(&o.Unparsed, "offer_request", out.OfferRequest.Raw)
	keepRaw(&o.Unparsed, "offer", out.Offer.Raw)
	return o, nil
}

// RespondOffer accepts or rejects the latest pending offer request a buyer
// sent in the conversation. Only the seller can.
func (c *Client) RespondOffer(ctx context.Context, convID int64, accept bool) (Offer, error) {
	d, err := c.deal(ctx, convID)
	if err != nil {
		return Offer{}, err
	}
	if !d.seller {
		return Offer{}, Usage("only the seller can accept or decline offers in conversation %d", convID)
	}
	if d.pending == nil {
		if len(d.unparsed) > 0 {
			return Offer{}, NotFound("conversation %d has no pending offer the cli can read; vinted sent %s in a shape it does not know", convID, strings.Join(slices.Sorted(maps.Keys(d.unparsed)), ", "))
		}
		return Offer{}, NotFound("conversation %d has no pending offer", convID)
	}
	verb, status := "reject", "rejected"
	if accept {
		verb, status = "accept", "accepted"
	}
	path := "/api/v2/transactions/" + strconv.FormatInt(d.transaction, 10) + "/offer_requests/" + strconv.FormatInt(d.pending.ID, 10) + "/" + verb
	if _, err := c.do(ctx, request{method: http.MethodPut, path: path, auth: true, csrf: true}, nil); err != nil {
		return Offer{}, err
	}
	o := *d.pending
	o.Status = status
	o.Unparsed = d.unparsed
	return o, nil
}

// SetReservation reserves the conversation's item for the buyer in it, or
// clears the reservation. Only the seller can.
func (c *Client) SetReservation(ctx context.Context, convID int64, reserved bool) error {
	d, err := c.deal(ctx, convID)
	if err != nil {
		return err
	}
	if !d.seller {
		return Usage("only the seller can reserve the item in conversation %d", convID)
	}
	path := "/api/v2/transactions/" + strconv.FormatInt(d.transaction, 10) + "/reservation"
	_, err = c.do(ctx, request{method: http.MethodPut, path: path, body: map[string]any{"reserved": reserved}, auth: true, csrf: true}, nil)
	return err
}
