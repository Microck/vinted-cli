package vinted

import (
	"errors"
	"net/http"
	"testing"
)

func TestGatewayBaseFollowsTheWebApp(t *testing.T) {
	for base, want := range map[string]string{
		"https://www.vinted.com":    "https://api.www.vinted.com",
		"https://www.vinted.fr":     "https://api.vinted.fr",
		"https://www.vinted.co.uk/": "https://api.vinted.co.uk",
		"http://127.0.0.1:4567":     "http://127.0.0.1:4567/web/gateway",
	} {
		c := &Client{Base: base}
		if got := c.gatewayBase(); got != want {
			t.Errorf("gatewayBase(%s) = %s, want %s", base, got, want)
		}
	}
}

func TestValidationErrorsNameTheFields(t *testing.T) {
	c := &Client{Base: DefaultBase}
	raw := []byte(`{"code":99,"message":"Sorry, there are some errors","message_code":"validation_error","errors":[{"field":"price","value":"Price is too low"},{"field":"title","value":"Title is too short"}]}`)
	err := c.statusError("POST /api/v2/item_upload/items", http.StatusUnprocessableEntity, raw)
	var ve *Error
	if !errors.As(err, &ve) || ve.Kind != KindUsage || ve.Msg != "vinted rejected the input: price: Price is too low; title: Title is too short" {
		t.Fatalf("err = %#v", err)
	}
	// The gateway's string codes must not break decoding of the message.
	err = c.statusError("GET /search-bar", http.StatusBadRequest, []byte(`{"code":"BAD_REQUEST","message":"Bad request"}`))
	if !errors.As(err, &ve) || ve.Msg != "Bad request" {
		t.Fatalf("gateway err = %#v", err)
	}
}

func TestNewUUIDIsVersion4(t *testing.T) {
	a, b := newUUID(), newUUID()
	if a == b || len(a) != 36 || a[14] != '4' || a[8] != '-' {
		t.Fatalf("uuids %s %s", a, b)
	}
}
