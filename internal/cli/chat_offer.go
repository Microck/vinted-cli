package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/spf13/cobra"
)

var offerAmountRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

type offerView vinted.Offer

func (v offerView) Pretty(w io.Writer, color bool) {
	fmt.Fprintf(w, "%d: %s %.2f %s (%s)\n", v.Conversation, v.Kind, v.Amount, v.Currency, v.Status)
}

func parseOfferAmount(s string) (float64, error) {
	if !offerAmountRE.MatchString(s) {
		return 0, output.Usagef("AMOUNT must be a positive decimal with at most two fractional digits")
	}
	amount, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(amount, 0) || amount <= 0 {
		return 0, output.Usagef("AMOUNT must be a positive decimal with at most two fractional digits")
	}
	return amount, nil
}

func (a *App) chatOfferCmd() *cobra.Command {
	var currency string
	cmd := &cobra.Command{
		Use:   "offer CONV AMOUNT",
		Short: "Send a price offer, or accept/decline an incoming one",
		Long: `Send a price offer in the conversation's currency. AMOUNT is a positive
decimal with at most two fractional digits. As the buyer this is an offer
request; as the seller it is a counter offer. Sellers answer the latest
pending offer with:
  vinted chat offer accept CONV
  vinted chat offer decline CONV
Vinted checks its own minimum and daily limits and reports them as errors.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseConvID(args[0])
			if err != nil {
				return err
			}
			amount, err := parseOfferAmount(args[1])
			if err != nil {
				return err
			}
			if err := a.requireSession(); err != nil {
				return err
			}
			o, err := a.Client.SendOffer(cmd.Context(), id, amount, strings.ToUpper(currency))
			if err != nil {
				return err
			}
			return a.Printer.Print(offerView(o))
		},
	}
	cmd.Flags().StringVar(&currency, "currency", "", "currency code (default: the conversation's)")
	for _, accept := range []bool{true, false} {
		name, short := "decline", "Decline the latest pending offer (seller)"
		if accept {
			name, short = "accept", "Accept the latest pending offer (seller)"
		}
		cmd.AddCommand(&cobra.Command{
			Use: name + " CONV", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := parseConvID(args[0])
				if err != nil {
					return err
				}
				if err := a.requireSession(); err != nil {
					return err
				}
				o, err := a.Client.RespondOffer(cmd.Context(), id, accept)
				if err != nil {
					return err
				}
				return a.Printer.Print(offerView(o))
			},
		})
	}
	return cmd
}

type reserveResult struct {
	Action       string `json:"action"`
	Conversation int64  `json:"conversation"`
	OK           bool   `json:"ok"`
}

func (r reserveResult) Pretty(w io.Writer, color bool) {
	fmt.Fprintf(w, "%s for conversation %d\n", r.Action, r.Conversation)
}

func (a *App) chatReserveCmd() *cobra.Command {
	var off bool
	cmd := &cobra.Command{
		Use:   "reserve CONV",
		Short: "Reserve your item for the buyer in a conversation (or --off)",
		Long: `Mark the conversation's item as reserved for that buyer, as the seller's
"Mark as reserved" button does. Vinted ties a reservation to one buyer, so it
is set from the conversation, not the item. --off clears it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseConvID(args[0])
			if err != nil {
				return err
			}
			if err := a.requireSession(); err != nil {
				return err
			}
			if err := a.Client.SetReservation(cmd.Context(), id, !off); err != nil {
				return err
			}
			action := "reserved"
			if off {
				action = "unreserved"
			}
			return a.Printer.Print(reserveResult{Action: action, Conversation: id, OK: true})
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "clear the reservation")
	return cmd
}

// messageKey identifies a message across polls; status events carry no id.
func messageKey(m vinted.Message) string {
	if m.ID != 0 {
		return strconv.FormatInt(m.ID, 10)
	}
	return m.At.UTC().Format(time.RFC3339Nano) + "\x00" + m.Text
}

func (a *App) chatOpenCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "open CONV",
		Short: "Line-mode chat: polls for incoming messages, sends what you type",
		Long: `Line-mode chat for one conversation. Prints the recent history, then polls
for new messages every --interval (default 30s, floor 10s) while each line you
type is sent. /quit or Ctrl-D exits. Vinted's web app has no realtime channel
the CLI can use, so new messages show up within one interval. A block from
Vinted's bot protection ends the chat with exit 5.
With --format jsonl, incoming messages are printed as JSON objects instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseConvID(args[0])
			if err != nil {
				return err
			}
			if interval < minChatPoll {
				return output.Usagef("--interval must be at least %s: polling vinted faster gets the session blocked", minChatPoll)
			}
			if err := a.requireSession(); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			conv, err := a.Client.Conversation(ctx, id)
			if err != nil {
				return err
			}
			jsonl := a.Printer.Format == output.JSONL
			seen := map[string]bool{}
			for _, m := range conv.Messages {
				seen[messageKey(m)] = true
			}
			if !jsonl {
				history := conv
				if len(history.Messages) > 20 {
					history.Messages = history.Messages[len(history.Messages)-20:]
				}
				convView(history).Pretty(a.Stdout, a.Printer.Color)
				fmt.Fprintln(a.Stderr, output.Dim("type a message and press enter; /quit to leave", a.Printer.Color))
			}
			show := func(m vinted.Message) {
				if jsonl {
					_ = a.Printer.Print(m)
					return
				}
				printMessage(a.Stdout, a.Printer.Color, m, conv.WithUser.Login)
			}
			lines := make(chan string)
			go func() {
				defer close(lines)
				sc := bufio.NewScanner(a.Stdin)
				for sc.Scan() {
					select {
					case lines <- sc.Text():
					case <-ctx.Done():
						return
					}
				}
			}()
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-tick.C:
					// A renewal the engine ran on its own since the last poll
					// may have been blocked; record its cooldown first, so this
					// poll stops on it with exit 5 instead of on the paused session.
					a.reportEngineBlocks(ctx)
					// Each poll is its own command for the request budget.
					a.Gate.Reset()
					latest, err := a.Client.Conversation(ctx, id)
					if err != nil {
						var ve *vinted.Error
						if errors.As(err, &ve) && (ve.Kind == vinted.KindAuth || ve.Kind == vinted.KindNotFound || ve.Kind == vinted.KindBlocked) {
							return err
						}
						fmt.Fprintf(a.Stderr, "vinted: poll failed: %v\n", err)
						continue
					}
					for _, m := range latest.Messages {
						if k := messageKey(m); !seen[k] {
							seen[k] = true
							show(m)
						}
					}
				case line, ok := <-lines:
					if !ok || strings.TrimSpace(line) == "/quit" {
						return nil
					}
					text := strings.TrimSpace(line)
					if text == "" {
						continue
					}
					sent, err := a.Client.SendMessage(ctx, id, text)
					if err != nil {
						if stopsRun(err) {
							return err
						}
						fmt.Fprintf(a.Stderr, "vinted: send failed: %v\n", err)
						continue
					}
					seen[messageKey(sent)] = true
					if !jsonl {
						if sent.At.IsZero() {
							sent.At = time.Now()
						}
						sent.FromSelf = true
						printMessage(a.Stdout, a.Printer.Color, sent, conv.WithUser.Login)
					}
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "how often to poll for new messages (floor 10s)")
	return cmd
}

// minChatPoll is the fastest `chat open` may poll a conversation. Tests
// lower it.
var minChatPoll = 10 * time.Second
