package vinted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Chat uses the web app's "legacy" messaging API under /api/v2, the backend
// vinted.com serves to accounts not yet moved to the new messaging service.
// Accounts Vinted has not verified get code 115 (user_verification_required)
// from the inbox, which statusError turns into a KindAuth with instructions.

type rawChatUser struct {
	ID    flexInt `json:"id"`
	Login string  `json:"login"`
}

// rawConversationSummary is one inbox row.
type rawConversationSummary struct {
	ID           flexInt     `json:"id"`
	Description  string      `json:"description"`
	Unread       bool        `json:"unread"`
	UpdatedAt    flexTime    `json:"updated_at"`
	OppositeUser rawChatUser `json:"opposite_user"`
}

// rawMessage is one entry of a conversation's messages list. Only
// entity_type "message" carries a body; status and offer events carry a
// title (and subtitle) instead.
type rawMessage struct {
	ID          flexInt  `json:"id"`
	EntityType  string   `json:"entity_type"`
	CreatedAtTS flexTime `json:"created_at_ts"`
	CreatedAt   flexTime `json:"created_at"`
	Entity      struct {
		ID       flexInt `json:"id"`
		Body     string  `json:"body"`
		Title    string  `json:"title"`
		Subtitle string  `json:"subtitle"`
		UserID   flexInt `json:"user_id"`
		// Offer events only.
		Price    flexMoneyAmount `json:"price"`
		Currency string          `json:"currency"`
		Status   json.RawMessage `json:"status"`
	} `json:"entity"`
}

type rawConversation struct {
	ID            flexInt      `json:"id"`
	Unread        bool         `json:"unread"`
	ReadByCurrent *bool        `json:"read_by_current_user"`
	UpdatedAt     flexTime     `json:"updated_at"`
	Description   string       `json:"description"`
	OppositeUser  rawChatUser  `json:"opposite_user"`
	Messages      []rawMessage `json:"messages"`
	Transaction   *struct {
		ItemID     flexInt   `json:"item_id"`
		ItemTitle  string    `json:"item_title"`
		OfferPrice *rawMoney `json:"offer_price"`
	} `json:"transaction"`
}

// UnreadCount is the badge on the envelope icon.
func (c *Client) UnreadCount(ctx context.Context) (int, error) {
	var out struct {
		UnreadMsgCount int `json:"unread_msg_count"`
	}
	if err := c.getJSON(ctx, "/api/v2/conversations/stats", nil, true, &out); err != nil {
		return 0, err
	}
	return out.UnreadMsgCount, nil
}

// Inbox lists conversations, most recent first, without their messages.
func (c *Client) Inbox(ctx context.Context, page, perPage int) ([]Conversation, error) {
	var out struct {
		Conversations []rawConversationSummary `json:"conversations"`
	}
	if err := c.getJSON(ctx, "/api/v2/inbox", pageQuery(page, perPage), true, &out); err != nil {
		return nil, err
	}
	convs := make([]Conversation, 0, len(out.Conversations))
	for _, r := range out.Conversations {
		convs = append(convs, Conversation{
			ID:          int64(r.ID),
			WithUser:    ChatUser{ID: int64(r.OppositeUser.ID), Login: r.OppositeUser.Login},
			Unread:      r.Unread,
			UpdatedAt:   r.UpdatedAt.Time,
			LastMessage: r.Description,
		})
	}
	return convs, nil
}

// Conversation loads one conversation with its messages, oldest first.
func (c *Client) Conversation(ctx context.Context, id int64) (Conversation, error) {
	if id <= 0 {
		return Conversation{}, Usage("conversation id must be positive")
	}
	var out struct {
		Conversation rawConversation `json:"conversation"`
	}
	if err := c.getJSON(ctx, "/api/v2/conversations/"+strconv.FormatInt(id, 10), nil, true, &out); err != nil {
		return Conversation{}, notFoundAs(err, "conversation %d does not exist", id)
	}
	return c.normalizeConversation(out.Conversation), nil
}

func (c *Client) normalizeConversation(r rawConversation) Conversation {
	var me int64
	if c.Account != nil {
		me = c.Account.UserID()
	}
	conv := Conversation{
		ID:          int64(r.ID),
		WithUser:    ChatUser{ID: int64(r.OppositeUser.ID), Login: r.OppositeUser.Login},
		Unread:      r.Unread,
		UpdatedAt:   r.UpdatedAt.Time,
		LastMessage: r.Description,
	}
	if r.ReadByCurrent != nil {
		conv.Unread = !*r.ReadByCurrent
	}
	if t := r.Transaction; t != nil {
		conv.Item = ChatItem{ID: int64(t.ItemID), Title: t.ItemTitle, Price: t.OfferPrice.amount(), Currency: t.OfferPrice.currency()}
	}
	for _, m := range r.Messages {
		msg, ok := normalizeMessage(m, me, conv.WithUser.ID)
		if !ok {
			continue
		}
		conv.Messages = append(conv.Messages, msg)
		if msg.At.After(conv.UpdatedAt) {
			conv.UpdatedAt = msg.At
		}
	}
	if n := len(conv.Messages); n > 0 && conv.LastMessage == "" {
		conv.LastMessage = conv.Messages[n-1].Text
	}
	return conv
}

func normalizeMessage(m rawMessage, me, other int64) (Message, bool) {
	text := m.Entity.Body
	if text == "" {
		text = strings.TrimSpace(m.Entity.Title + "\n" + m.Entity.Subtitle)
	}
	if m.EntityType == "offer_request_message" || m.EntityType == "offer_message" {
		text = offerText(m, text)
	}
	if text == "" {
		return Message{}, false
	}
	id := int64(m.Entity.ID)
	if id == 0 {
		id = int64(m.ID)
	}
	at := m.CreatedAtTS.Time
	if at.IsZero() {
		at = m.CreatedAt.Time
	}
	from := int64(m.Entity.UserID)
	// System events have no author; they are never from self.
	fromSelf := from != 0 && ((me != 0 && from == me) || (me == 0 && other != 0 && from != other))
	return Message{ID: id, FromSelf: fromSelf, Text: text, At: at}, true
}

// offerText labels an offer event, whose title is only the amount ("$60.00"),
// so a transcript tells it apart from a typed message.
func offerText(m rawMessage, amount string) string {
	e := m.Entity
	if amount == "" && e.Price.Amount > 0 {
		amount = formatAmount(e.Price.Amount) + " " + firstNonEmpty(e.Currency, e.Price.Currency)
	}
	if amount == "" {
		return ""
	}
	kind := "offer request"
	if m.EntityType == "offer_message" {
		kind = "counter offer"
	}
	text := kind + ": " + strings.TrimSpace(amount)
	if s := offerStatus(e.Status); s != "" {
		text += " (" + s + ")"
	}
	return text
}

// SendMessage replies in an existing conversation.
func (c *Client) SendMessage(ctx context.Context, convID int64, text string) (Message, error) {
	if convID <= 0 {
		return Message{}, Usage("conversation id must be positive")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, Usage("the message is empty")
	}
	body := map[string]any{"reply": map[string]any{
		"body":                                   text,
		"photo_temp_uuids":                       nil,
		"is_personal_data_sharing_check_skipped": false,
	}}
	var out struct {
		Conversation *rawConversation `json:"conversation"`
		Reply        *struct {
			ID        flexInt  `json:"id"`
			Body      string   `json:"body"`
			CreatedAt flexTime `json:"created_at_ts"`
		} `json:"reply"`
	}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/conversations/" + strconv.FormatInt(convID, 10) + "/replies", body: body, auth: true, csrf: true}, &out)
	if err != nil {
		return Message{}, notFoundAs(err, "conversation %d does not exist", convID)
	}
	// The response echoes the conversation; the newest own message is ours.
	if out.Conversation != nil {
		conv := c.normalizeConversation(*out.Conversation)
		for i := len(conv.Messages) - 1; i >= 0; i-- {
			if m := conv.Messages[i]; m.FromSelf && m.Text == text {
				return m, nil
			}
		}
	}
	msg := Message{FromSelf: true, Text: text, At: time.Now()}
	if out.Reply != nil {
		msg.ID = int64(out.Reply.ID)
		if !out.Reply.CreatedAt.IsZero() {
			msg.At = out.Reply.CreatedAt.Time
		}
	}
	return msg, nil
}

// StartConversation messages an item's seller, as the item page's "Message
// seller" button does (it routes through /inbox/want_it, which creates the
// conversation the same way). Vinted answers the create call with the
// existing conversation when there already is one about the item, so a second
// start lands in the same thread.
func (c *Client) StartConversation(ctx context.Context, itemID int64, text string) (Conversation, error) {
	if strings.TrimSpace(text) == "" {
		return Conversation{}, Usage("the message is empty")
	}
	if _, err := c.selfID(); err != nil {
		return Conversation{}, err
	}
	item, err := c.Item(ctx, itemID)
	if err != nil {
		return Conversation{}, err
	}
	if item.SellerID == 0 {
		return Conversation{}, &Error{Kind: KindAPIChanged, Endpoint: "GET /items/" + strconv.FormatInt(itemID, 10), Msg: "could not find the seller of the item"}
	}
	body := map[string]any{"initiator": "seller_enters_notification", "item_id": itemID, "opposite_user_id": item.SellerID}
	var out struct {
		Conversation rawConversation `json:"conversation"`
	}
	if _, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/conversations", body: body, auth: true, csrf: true}, &out); err != nil {
		return Conversation{}, err
	}
	convID := int64(out.Conversation.ID)
	if convID == 0 {
		return Conversation{}, &Error{Kind: KindAPIChanged, Endpoint: "POST /api/v2/conversations", Msg: "vinted did not return the new conversation"}
	}
	conv := c.normalizeConversation(out.Conversation)
	msg, err := c.SendMessage(ctx, convID, text)
	if err != nil {
		var ve *Error
		if errors.As(err, &ve) {
			ve.Msg = fmt.Sprintf("conversation %d with the seller exists, but the message was not sent: %s", convID, ve.Msg)
		}
		return Conversation{}, err
	}
	conv.Messages = append(conv.Messages, msg)
	conv.LastMessage = msg.Text
	if conv.Item.ID == 0 {
		conv.Item = ChatItem{ID: item.ID, Title: item.Title, Price: item.Price, Currency: item.Currency}
	}
	if conv.WithUser.ID == 0 {
		conv.WithUser = ChatUser{ID: item.SellerID, Login: item.SellerLogin}
	}
	return conv, nil
}
