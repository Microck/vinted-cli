package cli

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
)

func (a *App) chatCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "Read and send Vinted messages",
		Long: `Read and send Vinted messages.

CONV is a numeric conversation id, as listed by "vinted chat list".

Examples:
  vinted chat list --unread --format pretty
  vinted chat show 1234567890 --format pretty
  vinted chat send 1234567890 "Is it still available?"
  vinted chat start 4815162342 "Hi, would you ship today?"
  vinted chat open 1234567890
  vinted chat offer 1234567890 20
  echo "Would you take 20?" | vinted chat send 1234567890 -`,
	}
	cmd.AddCommand(a.chatListCmd(), a.chatShowCmd(), a.chatSendCmd(), a.chatStartCmd(),
		a.chatOpenCmd(), a.chatOfferCmd(), a.chatReserveCmd())
	return cmd
}

func parseConvID(ref string) (int64, error) {
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return 0, output.Usagef("%q is not a conversation id. See `vinted chat list`", ref)
	}
	return id, nil
}

type inboxView []vinted.Conversation

func (v inboxView) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(v))
	for _, c := range v {
		unread := ""
		if c.Unread {
			unread = "*"
		}
		price := ""
		if c.Item.Price > 0 {
			price = fmt.Sprintf("%.2f %s", c.Item.Price, c.Item.Currency)
		}
		rows = append(rows, []string{fmt.Sprint(c.ID), unread, output.Truncate(c.WithUser.Login, 16), output.Truncate(c.Item.Title, 28), price, output.Truncate(c.LastMessage, 40), output.Dim(relTime(c.UpdatedAt), color)})
	}
	output.Table(w, color, []string{"CONV", "NEW", "WITH", "ITEM", "PRICE", "LAST", "WHEN"}, rows)
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return t.Format("2006-01-02")
}

func (a *App) chatListCmd() *cobra.Command {
	var unread bool
	var limit int
	cmd := &cobra.Command{
		Use: "list", Short: "List conversations, most recent first", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			if limit < 1 {
				return output.Usagef("--limit starts at 1")
			}
			convs, err := a.Client.Inbox(cmd.Context(), 1, limit)
			if err != nil {
				return err
			}
			kept := []vinted.Conversation{}
			for _, c := range convs {
				if !unread || c.Unread {
					kept = append(kept, c)
				}
			}
			return a.Printer.Print(inboxView(kept))
		},
	}
	cmd.Flags().BoolVar(&unread, "unread", false, "only conversations with unread messages")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many conversations")
	return cmd
}

type convView vinted.Conversation

func (v convView) Pretty(w io.Writer, color bool) {
	c := vinted.Conversation(v)
	fmt.Fprintf(w, "%d  with %s", c.ID, c.WithUser.Login)
	if c.Item.Title != "" {
		fmt.Fprintf(w, "  about %s (%.2f %s)", c.Item.Title, c.Item.Price, c.Item.Currency)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	for _, m := range c.Messages {
		printMessage(w, color, m, c.WithUser.Login)
	}
}

func printMessage(w io.Writer, color bool, m vinted.Message, other string) {
	who := other
	if m.FromSelf {
		who = "me"
	}
	fmt.Fprintf(w, "%s %s: %s\n", output.Dim(m.At.Local().Format("01-02 15:04"), color), who, m.Text)
}

func (a *App) chatShowCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use: "show CONV", Short: "Show a conversation's messages, oldest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			id, err := parseConvID(args[0])
			if err != nil {
				return err
			}
			conv, err := a.Client.Conversation(cmd.Context(), id)
			if err != nil {
				return err
			}
			if limit > 0 && len(conv.Messages) > limit {
				conv.Messages = conv.Messages[len(conv.Messages)-limit:]
			}
			return a.Printer.Print(convView(conv))
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 30, "how many of the newest messages to show (0 = all)")
	return cmd
}

type sentView struct {
	Conversation int64  `json:"conversation"`
	MessageID    int64  `json:"message_id,omitempty"`
	Text         string `json:"text"`
	To           string `json:"to,omitempty"`
}

func (s sentView) Pretty(w io.Writer, color bool) {
	fmt.Fprintf(w, "sent to %s in %d\n", s.To, s.Conversation)
}

func (a *App) chatSendCmd() *cobra.Command {
	return &cobra.Command{
		Use: "send CONV TEXT", Short: "Send a message into an existing conversation (TEXT - reads stdin)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			id, err := parseConvID(args[0])
			if err != nil {
				return err
			}
			text, err := a.readArgOrStdin(args[1])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			conv, err := a.Client.Conversation(ctx, id)
			if err != nil {
				return err
			}
			m, err := a.Client.SendMessage(ctx, id, text)
			if err != nil {
				return err
			}
			return a.Printer.Print(sentView{Conversation: id, MessageID: m.ID, Text: text, To: conv.WithUser.Login})
		},
	}
}

func (a *App) chatStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start ITEM TEXT",
		Short: "Message an item's seller (Vinted reuses the conversation about it if one exists)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			itemID, err := vinted.ParseItemRef(args[0])
			if err != nil {
				return err
			}
			text, err := a.readArgOrStdin(args[1])
			if err != nil {
				return err
			}
			conv, err := a.Client.StartConversation(cmd.Context(), itemID, text)
			if err != nil {
				return err
			}
			var msgID int64
			if n := len(conv.Messages); n > 0 {
				msgID = conv.Messages[n-1].ID
			}
			return a.Printer.Print(sentView{Conversation: conv.ID, MessageID: msgID, Text: text, To: conv.WithUser.Login})
		},
	}
}
