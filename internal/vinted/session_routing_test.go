package vinted

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Microck/vinted-cli/internal/testutil"
)

type unavailableAccount struct{ sends, renews int }

func (a *unavailableAccount) Send(context.Context, string, string, http.Header, []byte) (int, http.Header, []byte, error) {
	a.sends++
	return 0, nil, nil, errors.New("stored engine session: protocol_error")
}
func (a *unavailableAccount) Renew(context.Context) error {
	a.renews++
	return errors.New("must not renew")
}
func (*unavailableAccount) UserID() int64 { return 3000000001 }

func TestPublicReadsIgnoreStoredAccount(t *testing.T) {
	f, c := newFake(t)
	a := &unavailableAccount{}
	c.Account = a
	ctx := context.Background()
	checks := []struct {
		name string
		run  func() error
	}{
		{"search", func() error { _, err := c.Search(ctx, SearchParams{Text: "pixel"}); return err }},
		{"categories", func() error { _, err := c.Categories(ctx); return err }},
		{"brands", func() error { _, err := c.Brands(ctx, "nike"); return err }},
		{"item", func() error { _, err := c.Item(ctx, 1000000001); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if a.sends != 0 || a.renews != 0 {
		t.Fatalf("public reads used account: %+v", a)
	}
	if f.count("GET /") != 1 {
		t.Fatalf("guest bootstrap count: %d", f.count("GET /"))
	}
	if c.Account != a {
		t.Fatal("stored account was detached")
	}
	if _, err := c.Me(ctx); err == nil {
		t.Fatal("account read silently fell back to guest")
	}
	if a.sends != 1 {
		t.Fatalf("account read did not use engine: %+v", a)
	}
}

func TestGuest401DoesNotRenewStoredAccount(t *testing.T) {
	f, c := newFake(t)
	a := &unavailableAccount{}
	c.Account = a
	calls := 0
	f.override["GET /api/v2/brands"] = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(fixture(t, "brands.json"))
	}
	if _, err := c.Brands(context.Background(), "nike"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || f.count("GET /") != 2 || a.sends != 0 || a.renews != 0 {
		t.Fatalf("calls=%d bootstraps=%d account=%+v", calls, f.count("GET /"), a)
	}
}

func TestPublicReadsWithStoredAccountKeepProxyAndCooldown(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		f, c := newFake(t)
		a := &unavailableAccount{}
		c.Account = a
		proxy, relayed := testutil.ForwardProxy(t)
		u, _ := ParseProxy(proxy.URL)
		c.UseProxy(u)
		if _, err := c.Search(context.Background(), SearchParams{}); err != nil {
			t.Fatal(err)
		}
		if relayed.Load() != 2 || a.sends != 0 {
			t.Fatalf("proxy=%d account=%+v", relayed.Load(), a)
		}
		u, _ = ParseProxy("socks5h://" + testutil.ClosedAddr(t))
		c.UseProxy(u)
		if _, err := c.Search(context.Background(), SearchParams{}); kindOf(err) != KindNetwork {
			t.Fatalf("closed proxy: %v", err)
		}
		if f.count("GET /catalog") != 1 {
			t.Fatal("guest request escaped the configured proxy")
		}
	})
	t.Run("cooldown", func(t *testing.T) {
		f, c := newFake(t)
		a := &unavailableAccount{}
		c.Account = a
		c.Gate = &recordingGate{refuse: &Error{Kind: KindBlocked, Msg: "cooldown active"}}
		if _, err := c.Search(context.Background(), SearchParams{}); kindOf(err) != KindBlocked {
			t.Fatalf("cooldown: %v", err)
		}
		if f.count("GET /")+f.count("GET /catalog") != 0 || a.sends != 0 {
			t.Fatal("cooldown allowed traffic")
		}
	})
	t.Run("block is not retried", func(t *testing.T) {
		f, c := newFake(t)
		a := &unavailableAccount{}
		c.Account = a
		gate := &recordingGate{}
		c.Gate = gate
		f.override["GET /catalog"] = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }
		if _, err := c.Search(context.Background(), SearchParams{}); kindOf(err) != KindBlocked {
			t.Fatalf("block: %v", err)
		}
		if f.count("GET /catalog") != 1 || len(gate.blocks) != 1 || a.sends != 0 || a.renews != 0 {
			t.Fatal("block was bypassed or retried")
		}
	})
}
