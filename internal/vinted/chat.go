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

// rawUserRef is the {id, login} stub Vinted embeds for another member: the
// other side of a conversation, an item's seller, a review's author.
type rawUserRef struct {
	ID    flexInt `json:"id"`
	Login string  `json:"login"`
}

// rawConversationSummary is one inbox row.
type rawConversationSummary struct {
	ID           flexInt             `json:"id"`
	Description  string              `json:"description"`
	Unread       bool                `json:"unread"`
	UpdatedAt    flexTime            `json:"updated_at"`
	OppositeUser lenient[rawUserRef] `json:"opposite_user"`
}

// rawMessage is one entry of a conversation's messages list. Only
// entity_type "message" carries a body; status and offer events carry a
// title (and subtitle) instead. The entity's shape depends on the entity
// type, so it is read leniently.
type rawMessage struct {
	ID          flexInt                   `json:"id"`
	EntityType  string                    `json:"entity_type"`
	CreatedAtTS flexTime                  `json:"created_at_ts"`
	CreatedAt   flexTime                  `json:"created_at"`
	Entity      lenient[rawMessageEntity] `json:"entity"`
}

type rawMessageEntity struct {
	ID       flexInt `json:"id"`
	Body     string  `json:"body"`
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle"`
	UserID   flexInt `json:"user_id"`
	// Offer events only.
	Price    flexMoneyAmount `json:"price"`
	Currency string          `json:"currency"`
	Status   json.RawMessage `json:"status"`
}

type rawConversation struct {
	ID            flexInt               `json:"id"`
	Unread        bool                  `json:"unread"`
	ReadByCurrent *bool                 `json:"read_by_current_user"`
	UpdatedAt     flexTime              `json:"updated_at"`
	Description   string                `json:"description"`
	OppositeUser  lenient[rawUserRef]   `json:"opposite_user"`
	Messages      []lenient[rawMessage] `json:"messages"`
	Transaction   lenient[*struct {
		ItemID     flexInt         `json:"item_id"`
		ItemTitle  string          `json:"item_title"`
		OfferPrice flexMoneyAmount `json:"offer_price"`
	}] `json:"transaction"`
}

// UnreadCount is the badge on the envelope icon.
func (c *Client) UnreadCount(ctx context.Context) (int, error) {
	var out struct {
		UnreadMsgCount flexInt `json:"unread_msg_count"`
	}
	if err := c.getJSON(ctx, "/api/v2/conversations/stats", nil, true, &out); err != nil {
		return 0, err
	}
	return int(out.UnreadMsgCount), nil
}

// Inbox lists conversations, most recent first, without their messages. A
// row in an unknown shape is kept whole in its entry's Unparsed.
func (c *Client) Inbox(ctx context.Context, page, perPage int) ([]Conversation, error) {
	var out struct {
		Conversations []lenient[rawConversationSummary] `json:"conversations"`
	}
	if err := c.getJSON(ctx, "/api/v2/inbox", pageQuery(page, perPage), true, &out); err != nil {
		return nil, err
	}
	convs := make([]Conversation, 0, len(out.Conversations))
	for i, lr := range out.Conversations {
		var conv Conversation
		if lr.Raw != nil {
			keepRaw(&conv.Unparsed, "conversations."+strconv.Itoa(i), lr.Raw)
			convs = append(convs, conv)
			continue
		}
		r := lr.V
		conv = Conversation{
			ID:          int64(r.ID),
			WithUser:    ChatUser{ID: int64(r.OppositeUser.V.ID), Login: r.OppositeUser.V.Login},
			Unread:      r.Unread,
			UpdatedAt:   r.UpdatedAt.Time,
			LastMessage: r.Description,
		}
		keepRaw(&conv.Unparsed, "opposite_user", r.OppositeUser.Raw)
		convs = append(convs, conv)
	}
	return convs, nil
}

// Conversation loads one conversation with its messages, oldest first.
func (c *Client) Conversation(ctx context.Context, id int64) (Conversation, error) {
	if id <= 0 {
		return Conversation{}, Usage("conversation id must be positive")
	}
	path := "/api/v2/conversations/" + strconv.FormatInt(id, 10)
	var out struct {
		Conversation rawConversation `json:"conversation"`
	}
	if err := c.getJSON(ctx, path, nil, true, &out); err != nil {
		return Conversation{}, notFoundAs(err, "conversation %d does not exist", id)
	}
	if out.Conversation.ID == 0 {
		return Conversation{}, &Error{Kind: KindAPIChanged, Endpoint: "GET " + path, Msg: "vinted returned a conversation without its id"}
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
		WithUser:    ChatUser{ID: int64(r.OppositeUser.V.ID), Login: r.OppositeUser.V.Login},
		Unread:      r.Unread,
		UpdatedAt:   r.UpdatedAt.Time,
		LastMessage: r.Description,
	}
	keepRaw(&conv.Unparsed, "opposite_user", r.OppositeUser.Raw)
	keepRaw(&conv.Unparsed, "transaction", r.Transaction.Raw)
	if r.ReadByCurrent != nil {
		conv.Unread = !*r.ReadByCurrent
	}
	if t := r.Transaction.V; t != nil {
		conv.Item = ChatItem{ID: int64(t.ItemID), Title: t.ItemTitle, Price: t.OfferPrice.Amount, Currency: t.OfferPrice.Currency}
		keepRaw(&conv.Unparsed, "transaction.offer_price", t.OfferPrice.Raw)
	}
	for i, lm := range r.Messages {
		if lm.Raw != nil {
			keepRaw(&conv.Unparsed, "messages."+strconv.Itoa(i), lm.Raw)
			continue
		}
		msg, ok := normalizeMessage(lm.V, me, conv.WithUser.ID)
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

// normalizeMessage reads one message. ok is false for an event with nothing
// to show; an entity in an unknown shape is kept in Unparsed instead.
func normalizeMessage(m rawMessage, me, other int64) (Message, bool) {
	e := m.Entity.V
	var unparsed map[string]json.RawMessage
	keepRaw(&unparsed, "entity", m.Entity.Raw)
	keepRaw(&unparsed, "entity.price", e.Price.Raw)
	text := e.Body
	if text == "" {
		text = strings.TrimSpace(e.Title + "\n" + e.Subtitle)
	}
	if m.EntityType == "offer_request_message" || m.EntityType == "offer_message" {
		text = offerText(m, text)
	}
	if text == "" && unparsed == nil {
		return Message{}, false
	}
	id := int64(e.ID)
	if id == 0 {
		id = int64(m.ID)
	}
	at := m.CreatedAtTS.Time
	if at.IsZero() {
		at = m.CreatedAt.Time
	}
	from := int64(e.UserID)
	// System events have no author; they are never from self.
	fromSelf := from != 0 && ((me != 0 && from == me) || (me == 0 && other != 0 && from != other))
	return Message{ID: id, FromSelf: fromSelf, Text: text, At: at, Unparsed: unparsed}, true
}

// offerText labels an offer event, whose title is only the amount ("$60.00"),
// so a transcript tells it apart from a typed message.
func offerText(m rawMessage, amount string) string {
	e := m.Entity.V
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
	// The message is sent once Vinted answers 2xx; the echo only enriches
	// it, so an echo in an unknown shape must not turn the send into an error.
	var out struct {
		Conversation lenient[*rawConversation] `json:"conversation"`
		Reply        lenient[*struct {
			ID        flexInt  `json:"id"`
			Body      string   `json:"body"`
			CreatedAt flexTime `json:"created_at_ts"`
		}] `json:"reply"`
	}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/conversations/" + strconv.FormatInt(convID, 10) + "/replies", body: body, auth: true, csrf: true}, &out)
	if err != nil {
		return Message{}, notFoundAs(err, "conversation %d does not exist", convID)
	}
	// The response echoes the conversation; the newest own message is ours.
	if out.Conversation.V != nil {
		conv := c.normalizeConversation(*out.Conversation.V)
		for i := len(conv.Messages) - 1; i >= 0; i-- {
			if m := conv.Messages[i]; m.FromSelf && m.Text == text {
				return m, nil
			}
		}
	}
	// Not found in the echo: the message is built from the request, and an
	// unreadable echo is kept since it holds the message's id and time.
	msg := Message{FromSelf: true, Text: text, At: time.Now()}
	keepRaw(&msg.Unparsed, "conversation", out.Conversation.Raw)
	keepRaw(&msg.Unparsed, "reply", out.Reply.Raw)
	if r := out.Reply.V; r != nil {
		msg.ID = int64(r.ID)
		if !r.CreatedAt.IsZero() {
			msg.At = r.CreatedAt.Time
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
