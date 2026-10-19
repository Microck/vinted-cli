package vinted

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Vinted mixes representations freely: ids are numbers in /api/v2 and
// strings in page payloads, prices are "6.0" strings, and timestamps are
// RFC 3339 strings or epoch seconds. These decoders accept every variant seen.

// lenient decodes an optional part of a payload into T, and keeps the raw
// JSON instead of failing when the part has a shape T does not describe.
// One odd optional field then cannot fail the whole response, and the data
// it carried still reaches the output through Raw (see Unparsed).
type lenient[T any] struct {
	V   T
	Raw json.RawMessage
}

func (l *lenient[T]) UnmarshalJSON(b []byte) error {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		l.Raw = append(json.RawMessage(nil), b...)
		return nil
	}
	l.V = v
	return nil
}

// keepRaw records raw under key in *dst when it is set, for an output's
// Unparsed map.
func keepRaw(dst *map[string]json.RawMessage, key string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	if *dst == nil {
		*dst = map[string]json.RawMessage{}
	}
	(*dst)[key] = raw
}

// flexInt decodes 123, "123" or null.
type flexInt int64

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		*n = flexInt(v)
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = flexInt(int64(f))
	}
	return nil
}

// flexFloat decodes 6, 6.5, "6.50" or null.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(bytes.TrimSpace(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64); err == nil {
		*f = flexFloat(v)
	}
	return nil
}

// flexTime decodes an RFC 3339 string or epoch seconds/milliseconds.
type flexTime struct{ time.Time }

func (t *flexTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) != nil || s == "" {
			return nil
		}
		if v, err := time.Parse(time.RFC3339, s); err == nil {
			t.Time = v
			return nil
		}
		b = []byte(s)
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n == 0 {
		return nil
	}
	if n > 1e12 {
		t.Time = time.UnixMilli(n)
	} else {
		t.Time = time.Unix(n, 0)
	}
	return nil
}

// rawMoney is {"amount":"6.0","currency_code":"USD"} (API) or
// {"amount":"6.00","currencyCode":"USD"} (pages).
type rawMoney struct {
	Amount       flexFloat `json:"amount"`
	CurrencyCode string    `json:"currency_code"`
	CurrencyCC   string    `json:"currencyCode"`
}

func (m *rawMoney) currency() string {
	if m == nil {
		return ""
	}
	if m.CurrencyCode != "" {
		return m.CurrencyCode
	}
	return m.CurrencyCC
}

func (m *rawMoney) amount() float64 {
	if m == nil {
		return 0
	}
	return float64(m.Amount)
}

// rawPhoto covers both the API and the page photo shapes.
type rawPhoto struct {
	URL            string `json:"url"`
	FullSizeURL    string `json:"full_size_url"`
	IsMain         bool   `json:"is_main"`
	HighResolution *struct {
		Timestamp flexTime `json:"timestamp"`
	} `json:"high_resolution"`
}

func photoURLs(photos []rawPhoto) []string {
	out := make([]string, 0, len(photos))
	for _, p := range photos {
		u := p.FullSizeURL
		if u == "" {
			u = p.URL
		}
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

// photoTime is the main photo's upload time, the closest thing to a listing
// date Vinted exposes for other people's items.
func photoTime(photos []rawPhoto) time.Time {
	for _, p := range photos {
		if p.HighResolution != nil && !p.HighResolution.Timestamp.IsZero() {
			return p.HighResolution.Timestamp.Time
		}
	}
	return time.Time{}
}

type rawPagination struct {
	CurrentPage  int `json:"current_page"`
	TotalPages   int `json:"total_pages"`
	TotalEntries int `json:"total_entries"`
	PerPage      int `json:"per_page"`
}

// splitSecondLine reads the item box's "M · Good" line: size then condition,
// or only the condition for items without a size.
func splitSecondLine(s string) (size, condition string) {
	parts := strings.Split(s, "·")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return "", parts[0]
	}
	return parts[0], parts[len(parts)-1]
}
