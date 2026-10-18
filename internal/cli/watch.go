package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/sink"
	"github.com/Microck/vinted-cli/internal/store"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/Microck/vinted-cli/internal/watch"
)

const (
	// Vinted's bot protection blocks sessions that poll it often, so no
	// schedule may check more often than every 15 minutes.
	defaultInterval = 15 * time.Minute
	minInterval     = 15 * time.Minute
	// maxSearchPages caps the result pages one search command or search
	// watch check reads.
	maxSearchPages = watch.MaxSearchPages
)

func (a *App) watchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Track searches, items and sellers and get events when they change",
		Long: `Track searches, items and sellers and get events when they change.

A watch is local. "check" runs every due watch once, prints new events and
delivers them to the watch's sinks; "run" loops in the foreground; "service"
installs a systemd timer or launchd agent that runs "check" on a schedule.

Examples:
  vinted watch add search "levis 501" --price-to 30 --name levis --notify phone
  vinted watch add item https://www.vinted.com/items/4815162342-levis-501 --name those-jeans
  vinted watch add seller 123456789 --name good-seller
  vinted watch add search "nike air max" --price-to 40 --name airmax --emit-initial   # seed a feed
  vinted watch check --all --format jsonl | jq -r 'select(.type=="item.new") | .item.url'
  vinted watch run --interval 30m
  vinted watch service install --interval 30m`,
	}
	add := &cobra.Command{Use: "add", Short: "Create a watch"}
	add.AddCommand(a.watchAddSearchCmd(), a.watchAddItemCmd(), a.watchAddSellerCmd())
	cmd.AddCommand(add, a.watchListCmd(), a.watchRemoveCmd(), a.watchCheckCmd(), a.watchRunCmd(), a.watchEventsCmd(), a.watchServiceCmd())
	return cmd
}

type watchAddFlags struct {
	name        string
	notify      []string
	interval    time.Duration
	pages       int
	emitInitial bool
}

func (a *App) bindWatchAddFlags(cmd *cobra.Command, f *watchAddFlags, pages bool) {
	cmd.Flags().StringVar(&f.name, "name", "", "watch name (required, used in events and commands)")
	cmd.Flags().StringSliceVar(&f.notify, "notify", nil, "sink names from config, repeatable")
	cmd.Flags().DurationVar(&f.interval, "interval", 0, "how often this watch is due (default from config, 15m; floor 15m)")
	if pages {
		cmd.Flags().IntVar(&f.pages, "pages", 1, fmt.Sprintf("result pages to diff per check (max %d)", maxSearchPages))
	}
	cmd.Flags().BoolVar(&f.emitInitial, "emit-initial", false, "report everything that exists now as events instead of baselining silently")
}

func (a *App) configuredInterval(flag time.Duration) (time.Duration, error) {
	return configuredWatchInterval(flag, a.Cfg.Watch.Interval)
}

// configuredWatchInterval resolves a schedule: the flag, else config's
// watch.interval, else the default, refusing anything under the floor.
func configuredWatchInterval(flag time.Duration, configured string) (time.Duration, error) {
	iv := flag
	if iv == 0 && configured != "" {
		d, err := time.ParseDuration(configured)
		if err != nil {
			return 0, output.Usagef("config watch.interval: %q is not a duration like 30m", configured)
		}
		iv = d
	}
	if iv == 0 {
		iv = defaultInterval
	}
	if iv < minInterval {
		return 0, output.Usagef("interval %s is below the floor of %s: checking vinted more often gets the session blocked", iv, minInterval)
	}
	return iv, nil
}

func (a *App) checkSinks(names []string) error {
	for _, n := range names {
		s, ok := a.Cfg.Sinks[n]
		if !ok {
			return output.Usagef("sink %q is not in config. Define [sinks.%s] in %s", n, n, a.Paths.ConfigFile)
		}
		if err := sink.Validate(n, s); err != nil {
			return output.Usagef("%v", err)
		}
	}
	return nil
}

// checkWatchAdd validates a watch's flags before anything talks to Vinted
// and returns its interval.
func (a *App) checkWatchAdd(f watchAddFlags) (time.Duration, error) {
	// --name is checked here rather than with MarkFlagRequired, whose error
	// cobra reports as a generic failure (exit 1) instead of a usage error.
	if f.name == "" {
		return 0, output.Usagef("--name is required: it identifies the watch in events and in `vinted watch check NAME`")
	}
	if !validWatchName(f.name) {
		return 0, output.Usagef("watch name %q must be letters, digits, - or _", f.name)
	}
	if f.pages < 0 || f.pages > maxSearchPages {
		return 0, output.Usagef("--pages must be between 1 and %d: every page is one more request to vinted per check", maxSearchPages)
	}
	iv, err := a.configuredInterval(f.interval)
	if err != nil {
		return 0, err
	}
	return iv, a.checkSinks(f.notify)
}

func (a *App) addWatch(ctx context.Context, kind string, target any, f watchAddFlags, iv time.Duration) error {
	raw, err := json.Marshal(target)
	if err != nil {
		return err
	}
	st, err := a.Store()
	if err != nil {
		return err
	}
	w, err := st.AddWatch(ctx, store.Watch{Name: f.name, Profile: a.Profile, Kind: kind, Target: raw, Sinks: f.notify, Interval: iv, Pages: f.pages})
	if errors.Is(err, store.ErrExists) {
		return output.Usagef("watch %q already exists. Remove it first or pick another name", f.name)
	}
	if err != nil {
		return err
	}
	// Baseline now so the first scheduled check only reports real changes.
	// With --emit-initial the baseline itself is reported and delivered. A
	// baseline that fails takes the watch back out: the command reported a
	// failure, so it must not leave a watch behind that blocks the retry with
	// "already exists".
	res, err := watch.Check(ctx, a.Client, st, w, f.emitInitial)
	if err != nil {
		_ = st.RemoveWatch(context.WithoutCancel(ctx), w.Name)
		return err
	}
	if f.emitInitial {
		for _, ev := range res.Events {
			for _, name := range w.Sinks {
				if err := sink.Deliver(ctx, name, a.Cfg.Sinks[name], ev); err != nil {
					fmt.Fprintf(a.Stderr, "vinted: sink %s: %v\n", name, err)
				}
			}
		}
	}
	if err := st.CommitCheck(ctx, w.ID, res.SinceID, res.Upserts, res.Removed, res.Events); err != nil {
		_ = st.RemoveWatch(context.WithoutCancel(ctx), w.Name)
		return err
	}
	if f.emitInitial {
		return a.Printer.Print(eventList(res.Events))
	}
	// Print the watch as stored, so the baseline's check time shows.
	if stored, err := st.Watch(ctx, w.Name); err == nil {
		w = stored
	}
	return a.Printer.Print(watchView{Watch: w, Seen: len(res.Upserts)})
}

func validWatchName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func (a *App) watchAddSearchCmd() *cobra.Command {
	var sf searchFlags
	var wf watchAddFlags
	var fromAlert string
	var searchFlagSet *pflag.FlagSet
	cmd := &cobra.Command{
		Use: "search [text...]", Short: "Watch a search for new items and price changes",
		Long: `Watch a search for new items and price changes. Takes the same text and
filters as ` + "`vinted search`" + `; --pages sets how many result pages each check diffs.
Without --order the watch reads newest_first, where new items show up.

Only items newer than the watch count as item.new: Vinted reshuffles its
result pages between loads, so an older item appearing on the page is
recorded without an event.

Or give --from-alert ID to copy the query of one of the account's saved
searches (see ` + "`vinted alert list`" + `). The two are exclusive: the saved query
is replayed as Vinted stored it, except that an unset order becomes newest_first.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			iv, err := a.checkWatchAdd(wf)
			if err != nil {
				return err
			}
			var p vinted.SearchParams
			if cmd.Flags().Changed("from-alert") {
				if len(args) > 0 || searchFlagsGiven(searchFlagSet) {
					return output.Usagef("--from-alert copies the saved search's query; drop the text and search flags")
				}
				p, err = a.savedSearchParams(cmd, fromAlert)
			} else {
				p, err = toParams(args, sf)
			}
			if err != nil {
				return err
			}
			if p.Order == "" {
				p.Order = "newest_first"
			}
			return a.addWatch(cmd.Context(), store.KindSearch, watch.SearchTarget{Params: p}, wf, iv)
		},
	}
	searchFlagSet = a.bindSearchFlags(cmd, &sf)
	a.bindWatchAddFlags(cmd, &wf, true)
	cmd.Flags().StringVar(&fromAlert, "from-alert", "", "copy the query of this saved search (see `vinted alert list`)")
	return cmd
}

func (a *App) watchAddItemCmd() *cobra.Command {
	var wf watchAddFlags
	cmd := &cobra.Command{
		Use: "item ITEM", Short: "Watch one item for price, reserved, sold, removed", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			iv, err := a.checkWatchAdd(wf)
			if err != nil {
				return err
			}
			it, err := a.item(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.addWatch(cmd.Context(), store.KindItem, watch.ItemTarget{ID: it.ID, URL: it.URL}, wf, iv)
		},
	}
	a.bindWatchAddFlags(cmd, &wf, false)
	return cmd
}

func (a *App) watchAddSellerCmd() *cobra.Command {
	var wf watchAddFlags
	cmd := &cobra.Command{
		Use: "seller USER", Short: "Watch a seller for new and removed items", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			iv, err := a.checkWatchAdd(wf)
			if err != nil {
				return err
			}
			id, err := vinted.ParseUserRef(args[0])
			if err != nil {
				return err
			}
			u, err := a.Client.User(cmd.Context(), id)
			if err != nil {
				return err
			}
			return a.addWatch(cmd.Context(), store.KindSeller, watch.SellerTarget{ID: id, Login: u.Login}, wf, iv)
		},
	}
	a.bindWatchAddFlags(cmd, &wf, false)
	return cmd
}

type watchView struct {
	store.Watch
	Seen int `json:"seen,omitempty"`
}

func describeTarget(w store.Watch) string {
	switch w.Kind {
	case store.KindSearch:
		var t watch.SearchTarget
		_ = json.Unmarshal(w.Target, &t)
		return describeParams(t.Params)
	case store.KindItem:
		var t watch.ItemTarget
		_ = json.Unmarshal(w.Target, &t)
		return fmt.Sprint(t.ID)
	case store.KindSeller:
		var t watch.SellerTarget
		_ = json.Unmarshal(w.Target, &t)
		return strings.TrimSpace(t.Login + " " + fmt.Sprint(t.ID))
	}
	return ""
}

func (v watchView) Pretty(w io.Writer, color bool) {
	watchList{v.Watch}.Pretty(w, color)
}

type watchList []store.Watch

func (l watchList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, wt := range l {
		last := "never"
		if !wt.LastCheckAt.IsZero() {
			last = relTime(wt.LastCheckAt)
		}
		rows = append(rows, []string{wt.Name, wt.Kind, output.Truncate(describeTarget(wt), 40), wt.Interval.String(), strings.Join(wt.Sinks, ","), last, output.Dim(wt.Profile, color)})
	}
	output.Table(w, color, []string{"WATCH", "KIND", "TARGET", "EVERY", "SINKS", "CHECKED", "PROFILE"}, rows)
}

func (a *App) watchListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use: "list", Short: "List watches for the active profile", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.Store()
			if err != nil {
				return err
			}
			profile := a.Profile
			if all {
				profile = ""
			}
			ws, err := st.Watches(cmd.Context(), profile)
			if err != nil {
				return err
			}
			if ws == nil {
				ws = []store.Watch{}
			}
			return a.Printer.Print(watchList(ws))
		},
	}
	cmd.Flags().BoolVar(&all, "all-profiles", false, "include every profile's watches")
	return cmd
}

func (a *App) watchRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use: "remove NAME", Short: "Delete a watch and its history", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.Store()
			if err != nil {
				return err
			}
			if err := a.confirm(yes, fmt.Sprintf("Remove watch %q and its event history?", args[0])); err != nil {
				return err
			}
			if err := st.RemoveWatch(cmd.Context(), args[0]); errors.Is(err, store.ErrNotFound) {
				return vinted.NotFound("no watch named %q", args[0])
			} else if err != nil {
				return err
			}
			return a.Printer.Print(map[string]string{"removed": args[0]})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not prompt")
	return cmd
}

type eventList []store.Event

func (l eventList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, ev := range l {
		title, body, link := sink.Summary(ev)
		rows = append(rows, []string{output.Dim(ev.At.Local().Format("01-02 15:04"), color), ev.Watch, ev.Type, output.Truncate(title, 40), output.Truncate(body, 24), output.Dim(link, color)})
	}
	output.Table(w, color, []string{"WHEN", "WATCH", "EVENT", "ITEM", "DETAIL", "URL"}, rows)
}

// runChecks checks the given watches one after another (each request still
// waits its turn at the gate), delivers events, commits, and returns every
// event produced. Per-watch failures are reported on stderr and do not stop
// the others; the first failure is returned so the exit code reflects it. A
// block or an exhausted request budget stops the run: every further watch
// would only fail the same way.
func (a *App) runChecks(ctx context.Context, st *store.Store, ws []store.Watch) ([]store.Event, error) {
	var all []store.Event
	var firstErr error
	for _, w := range ws {
		res, err := watch.Check(ctx, a.Client, st, w, false)
		if err != nil {
			if ctx.Err() != nil {
				return all, ctx.Err()
			}
			if stopsRun(err) || a.Gate.Exhausted() {
				return all, fmt.Errorf("watch %s: %w", w.Name, err)
			}
			fmt.Fprintf(a.Stderr, "vinted: watch %s: %v\n", w.Name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, ev := range res.Events {
			for _, name := range w.Sinks {
				if err := sink.Deliver(ctx, name, a.Cfg.Sinks[name], ev); err != nil {
					fmt.Fprintf(a.Stderr, "vinted: sink %s: %v\n", name, err)
				}
			}
		}
		if err := st.CommitCheck(ctx, w.ID, res.SinceID, res.Upserts, res.Removed, res.Events); err != nil {
			return all, err
		}
		all = append(all, res.Events...)
	}
	return all, firstErr
}

// stopsRun reports errors that end a whole check run or watch loop: a block
// (and the cooldown it starts) and a rejected session.
func stopsRun(err error) bool {
	var we *vinted.Error
	return errors.As(err, &we) && (we.Kind == vinted.KindBlocked || we.Kind == vinted.KindAuth)
}

// selectWatches picks named watches, or the due ones (or all with force).
func (a *App) selectWatches(ctx context.Context, st *store.Store, names []string, force bool) ([]store.Watch, error) {
	if len(names) > 0 {
		var out []store.Watch
		for _, n := range names {
			w, err := st.Watch(ctx, n)
			if errors.Is(err, store.ErrNotFound) {
				return nil, vinted.NotFound("no watch named %q", n)
			} else if err != nil {
				return nil, err
			}
			out = append(out, w)
		}
		return out, nil
	}
	ws, err := st.Watches(ctx, a.Profile)
	if err != nil {
		return nil, err
	}
	if force {
		return ws, nil
	}
	now := time.Now()
	due := ws[:0]
	for _, w := range ws {
		if w.LastCheckAt.IsZero() || now.Sub(w.LastCheckAt) >= w.Interval {
			due = append(due, w)
		}
	}
	return due, nil
}

func (a *App) watchCheckCmd() *cobra.Command {
	var all bool
	var jitter time.Duration
	cmd := &cobra.Command{
		Use:   "check [NAME...]",
		Short: "Run due watches once and print their events",
		Long: `Run watches once. With names, those watches run regardless of schedule. Without
names, watches whose interval has elapsed run; --all forces every watch.
Watches run one after another, and every request waits its turn behind the
shared request interval. Prints the events produced (none is fine: exit 0)
and delivers them to sinks. --jitter first sleeps a random time up to the
given duration, so scheduled runs do not hit Vinted at fixed times; the
service units pass a tenth of their interval.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jitter < 0 {
				return output.Usagef("--jitter cannot be negative")
			}
			// Rotate first so a run that fails below still cannot grow the log unbounded.
			a.rotateServiceLog()
			st, err := a.Store()
			if err != nil {
				return err
			}
			ws, err := a.selectWatches(cmd.Context(), st, args, all)
			if err != nil {
				return err
			}
			if jitter > 0 && len(ws) > 0 {
				if err := throttleClock.Sleep(cmd.Context(), rand.N(jitter)); err != nil {
					return err
				}
			}
			events, err := a.runChecks(cmd.Context(), st, ws)
			if !stopsRun(err) {
				a.keepSessionAlive(cmd.Context())
			}
			if events == nil {
				events = []store.Event{}
			}
			if perr := a.Printer.Print(eventList(events)); perr != nil {
				return perr
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "run every watch of the profile, due or not")
	cmd.Flags().DurationVar(&jitter, "jitter", 0, "sleep a random time up to this before checking (e.g. 90s)")
	return cmd
}

// keepSessionAlive attaches to the session engine once per check run, so its
// scheduler renews a stored session that is due while the check runs (and for
// the engine's short idle grace after it). Watches read public data, so a
// scheduled timer would otherwise leave the login to go stale. Failure is a
// warning: the watches themselves ran.
func (a *App) keepSessionAlive(ctx context.Context) {
	if a.Account == nil {
		return
	}
	meta, err := a.Account.status(ctx)
	switch {
	case err != nil && ctx.Err() == nil:
		fmt.Fprintf(a.Stderr, "vinted: session keepalive failed: %v\n", err)
	case err == nil && meta.Status != "ready" && meta.Status != "retry_wait":
		fmt.Fprintf(a.Stderr, "vinted: the session engine reports the session as %s; run `vinted auth status --check`\n", meta.Status)
	}
	if err != nil {
		return
	}
	// A renewal the engine ran on its own may have been blocked; no command
	// saw it, so start the shared cooldown here.
	if c, err := a.galletonClient(ctx); err == nil {
		if err := a.recordEngineBlocks(ctx, c, map[string]time.Time{}); err != nil && ctx.Err() == nil {
			fmt.Fprintf(a.Stderr, "vinted: could not check the session engine for blocked renewals: %v\n", err)
		}
	}
}

func (a *App) watchRunCmd() *cobra.Command {
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Check due watches in a loop until Ctrl-C",
		Long: `Foreground loop. Every tick runs the due watches one after another and prints
events as they happen (use --format jsonl to stream). The tick defaults to the
shortest watch interval, floored at 15m, with up to 10% jitter. A block from
Vinted's bot protection ends the loop with exit 5.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if interval != 0 && interval < minInterval {
				return output.Usagef("--interval %s is below the floor of %s: checking vinted more often gets the session blocked", interval, minInterval)
			}
			st, err := a.Store()
			if err != nil {
				return err
			}
			for {
				// Each tick is its own command for the request budget.
				a.Gate.Reset()
				ws, err := a.selectWatches(ctx, st, nil, false)
				if err != nil {
					return err
				}
				events, err := a.runChecks(ctx, st, ws)
				if ctx.Err() != nil {
					return nil
				}
				if stopsRun(err) {
					return err
				}
				a.keepSessionAlive(ctx)
				for _, ev := range events {
					if err := a.Printer.Print(ev); err != nil {
						return err
					}
				}
				tick := interval
				if tick == 0 {
					tick = a.shortestInterval(ctx, st)
				}
				tick += watch.Jitter(tick, time.Now().UnixNano())
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(tick):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 0, "tick length (default: shortest watch interval)")
	return cmd
}

func (a *App) shortestInterval(ctx context.Context, st *store.Store) time.Duration {
	ws, err := st.Watches(ctx, a.Profile)
	if err != nil || len(ws) == 0 {
		return defaultInterval
	}
	min := ws[0].Interval
	for _, w := range ws[1:] {
		if w.Interval < min {
			min = w.Interval
		}
	}
	if min < minInterval {
		return minInterval
	}
	return min
}

func (a *App) watchEventsCmd() *cobra.Command {
	var since time.Duration
	var limit int
	cmd := &cobra.Command{
		Use: "events [NAME]", Short: "Show stored events, newest first", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.Store()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			var from time.Time
			if since > 0 {
				from = time.Now().Add(-since)
			}
			evs, err := st.Events(cmd.Context(), a.Profile, name, from, limit)
			if err != nil {
				return err
			}
			if evs == nil {
				evs = []store.Event{}
			}
			return a.Printer.Print(eventList(evs))
		},
	}
	cmd.Flags().DurationVar(&since, "since", 0, "only events newer than this (e.g. 24h)")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum events")
	return cmd
}

// service: OS scheduler integration. No daemon; the scheduler runs `watch check`.

func (a *App) watchServiceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "service", Short: "Run checks on a schedule via systemd (Linux) or launchd (macOS)"}
	var interval time.Duration
	install := &cobra.Command{
		Use: "install", Short: "Install a user-level timer that runs `watch check --all`", Args: cobra.NoArgs,
		Long: `Install a user-level timer that runs "watch check --all" every --interval
(floor 15m). Each run first sleeps a random tenth of the interval, so checks
do not hit Vinted at fixed times.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			iv, err := a.configuredInterval(interval)
			if err != nil {
				return err
			}
			return a.serviceInstall(iv)
		},
	}
	install.Flags().DurationVar(&interval, "interval", 0, "timer period (default from config, 15m; floor 15m)")
	cmd.AddCommand(install,
		&cobra.Command{Use: "uninstall", Short: "Remove the timer and its log", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error { return a.serviceUninstall() }},
		&cobra.Command{Use: "status", Short: "Show the timer's state and last run", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error { return a.serviceStatus() }},
	)
	return cmd
}

type serviceView struct {
	Platform string   `json:"platform"`
	Unit     string   `json:"unit,omitempty"`
	Files    []string `json:"files,omitempty"`
	Command  string   `json:"command,omitempty"`
	State    string   `json:"state,omitempty"`
	LastRun  string   `json:"last_run,omitempty"`
	Log      string   `json:"log,omitempty"`
	Hint     string   `json:"hint,omitempty"`
}

func (v serviceView) Pretty(w io.Writer, color bool) {
	rows := [][]string{{"platform", v.Platform}}
	if v.Unit != "" {
		rows = append(rows, []string{"unit", v.Unit})
	}
	for _, f := range v.Files {
		rows = append(rows, []string{"file", f})
	}
	if v.State != "" {
		rows = append(rows, []string{"state", v.State})
	}
	if v.LastRun != "" {
		rows = append(rows, []string{"last run", v.LastRun})
	}
	if v.Log != "" {
		rows = append(rows, []string{"log", v.Log})
	}
	if v.Command != "" {
		rows = append(rows, []string{"command", v.Command})
	}
	output.Table(w, color, nil, rows)
	if v.Hint != "" {
		fmt.Fprintln(w, v.Hint)
	}
}

func (a *App) serviceUnitName() string { return "vinted-watch-" + a.Profile }

// serviceLogFile is where the scheduler appends this profile's check output.
func (a *App) serviceLogFile() string {
	return filepath.Join(a.Paths.StateDir, a.serviceUnitName()+".log")
}

// serviceUnit is this profile's scheduler integration on the current platform.
// One GOOS switch decides platform, unit name and unit files; everything else
// reads from here. Nil on platforms without integration (Windows gets a
// printed command instead).
type serviceUnit struct {
	platform string   // systemd or launchd
	name     string   // timer unit or launchd label
	files    []string // written by install; any missing means not installed
}

func (a *App) serviceUnit() *serviceUnit {
	n := a.serviceUnitName()
	switch runtime.GOOS {
	case "linux":
		dir := filepath.Join(config.UserHome(), ".config", "systemd", "user")
		return &serviceUnit{platform: "systemd", name: n + ".timer", files: []string{filepath.Join(dir, n+".service"), filepath.Join(dir, n+".timer")}}
	case "darwin":
		label := "dev.micr.vinted-cli." + a.Profile
		return &serviceUnit{platform: "launchd", name: label, files: []string{filepath.Join(config.UserHome(), "Library", "LaunchAgents", label+".plist")}}
	}
	return nil
}

func (u *serviceUnit) installed() bool {
	for _, f := range u.files {
		if _, err := os.Stat(f); err != nil {
			return false
		}
	}
	return true
}

// state asks the scheduler. "not installed" when the unit files are missing.
// On systemd an active timer that is not enabled dies at reboot, and a user
// manager without linger dies at logout; both are appended so "active" is
// never a false comfort. lastRun is systemd's last trigger time; launchd has
// no cheap equivalent.
func (u *serviceUnit) state() (state, lastRun string) {
	if !u.installed() {
		return "not installed", ""
	}
	if u.platform == "launchd" {
		if err := exec.Command("launchctl", "list", u.name).Run(); err != nil {
			return "not loaded", ""
		}
		return "loaded", ""
	}
	out, err := exec.Command("systemctl", "--user", "is-active", u.name).Output()
	state = strings.TrimSpace(string(out))
	if state == "" {
		if err != nil {
			return "unknown", ""
		}
		state = "inactive"
	}
	if enabled, _ := exec.Command("systemctl", "--user", "is-enabled", u.name).Output(); strings.TrimSpace(string(enabled)) != "enabled" {
		state += " (not enabled: gone after reboot)"
	}
	if !lingerEnabled() {
		state += " (linger off: stops at logout, run `loginctl enable-linger`)"
	}
	if last, err := exec.Command("systemctl", "--user", "show", "-p", "LastTriggerUSec", "--value", u.name).Output(); err == nil {
		if v := strings.TrimSpace(string(last)); v != "" && v != "n/a" {
			lastRun = v
		}
	}
	return state, lastRun
}

// lingerEnabled reports whether the user manager survives logout, which a
// user timer needs to run unattended and after a reboot. The user comes from
// the passwd database rather than $USER, which a scheduled or su'd process may
// not have: an empty name would make loginctl fail and every status line claim
// linger is off.
func lingerEnabled() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := exec.Command("loginctl", "show-user", "--value", "-p", "Linger", u.Username).Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// serviceLogCap bounds the scheduler log: a month of five-minute checks that
// print nothing is a few hundred KB, so 1 MiB means rotation only happens on
// a busy or noisy watch.
const serviceLogCap = 1 << 20

// rotateServiceLog moves an oversized scheduler log to ".1" at the start of a
// check. systemd and launchd hold the old file open in append mode, so the
// current run keeps writing to the renamed file and the next run starts a
// fresh one. Rotation happens here rather than in the unit files because the
// two schedulers have no shared size-cap primitive and a shell wrapper in the
// units would be harder to test. No log file means nothing to do.
func (a *App) rotateServiceLog() {
	log := a.serviceLogFile()
	info, err := os.Stat(log)
	if err != nil || info.Size() <= serviceLogCap {
		return
	}
	if err := os.Rename(log, log+".1"); err != nil {
		fmt.Fprintf(a.Stderr, "vinted: could not rotate %s: %v\n", log, err)
	}
}

// serviceCommandLine is what the scheduler runs: every watch, with a random
// start delay of up to a tenth of the interval.
func (a *App) serviceCommandLine(iv time.Duration) (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	args := []string{"watch", "check", "--all", "--jitter", (iv / 10).Round(time.Second).String(), "--profile", a.Profile, "--format", "jsonl"}
	return exe, args, nil
}

// These are the variables that decide where the CLI reads and writes and
// which backend it talks to. systemd and launchd
// start a job with a bare environment, so anyone whose shell points the CLI
// elsewhere would get a timer reading a different, empty state database from
// the one they installed from, checking nothing and reporting nothing, with no
// error anywhere. HOME is in the list because it is where the paths come from
// when the XDG variables are unset, which is the common case.
// No credential goes in: a unit file is world-readable (the engine's API
// token stays in its file; only the file's path goes in). The proxy variables
// go in, or the timer (and the session engine it starts) would talk to Vinted
// directly; install refuses one carrying credentials.
var (
	serviceEnvXDGKeys   = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}
	serviceEnvProxyKeys = []string{"VINTED_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"}
	// The external session engine selection and custom CA trust are paths and
	// URLs, not secrets; without them a background run would use another
	// engine (a vault without the session) or fail TLS.
	serviceEnvURLKeys = append([]string{"VINTED_BASE_URL", "VINTED_GALLETON_DIR", "VINTED_GALLETON_URL", "VINTED_GALLETON_TOKEN_FILE", "SSL_CERT_FILE", "SSL_CERT_DIR"}, serviceEnvProxyKeys...)
)

// systemdEnv renders the frozen environment as Environment= lines, and
// unsets every carried variable the enabling shell did not have: the user
// manager's own environment must not slip in a proxy or path the shell
// left out.
func systemdEnv(env [][2]string) string {
	var b strings.Builder
	set := map[string]bool{}
	for _, kv := range env {
		set[kv[0]] = true
		b.WriteString("Environment=" + systemdQuote(kv[0]+"="+kv[1]) + "\n")
	}
	var unset []string
	for _, k := range append(append(append([]string{}, serviceEnvXDGKeys...), "VINTED_CONFIG"), serviceEnvURLKeys...) {
		if !set[k] {
			unset = append(unset, k)
		}
	}
	if len(unset) > 0 {
		b.WriteString("UnsetEnvironment=" + strings.Join(unset, " ") + "\n")
	}
	return b.String()
}

// launchdEnv adds every carried variable the enabling shell did not have as
// an empty value: launchd has no unset, and the CLI, Go's proxy lookup and
// the XDG paths all treat an empty variable as unset, so `launchctl setenv`
// leftovers cannot slip in.
func launchdEnv(env [][2]string) [][2]string {
	set := map[string]bool{}
	for _, kv := range env {
		set[kv[0]] = true
	}
	out := append([][2]string{}, env...)
	for _, k := range append(append(append([]string{}, serviceEnvXDGKeys...), "VINTED_CONFIG"), serviceEnvURLKeys...) {
		if !set[k] {
			out = append(out, [2]string{k, ""})
		}
	}
	return out
}

// checkServiceProxies refuses to freeze a proxy carrying credentials into a
// world-readable unit file.
func checkServiceProxies() error {
	for _, k := range serviceEnvProxyKeys {
		raw := os.Getenv(k)
		if raw == "" || strings.HasPrefix(strings.ToLower(k), "no_proxy") {
			continue
		}
		if k != "VINTED_PROXY" && !strings.Contains(raw, "://") {
			// Go reads a scheme-less HTTPS_PROXY as http://.
			raw = "http://" + raw
		}
		u, err := vinted.ParseProxy(raw)
		if err != nil {
			if k == "VINTED_PROXY" {
				return output.Usagef("VINTED_PROXY: %v", err)
			}
			// Unusable for the CLI, but it still lands in the unit as it is.
			if strings.Contains(raw, "@") {
				return output.Usagef("%s looks like it carries credentials, which would land in a world-readable unit file. Use a proxy without credentials (for example a local socks5h forwarder) for scheduled and background runs", k)
			}
			continue
		}
		if u.User != nil {
			return output.Usagef("%s carries credentials, which would land in a world-readable unit file. Use a proxy without credentials (for example a local socks5h forwarder) for scheduled and background runs", k)
		}
	}
	return nil
}

// serviceEnv freezes the current values of those variables into the unit, so
// the scheduled run resolves the same paths as the install did. Each group has
// its own rule, because each is read differently:
//
//   - HOME goes in as it is. Install refuses a relative one before this runs.
//   - An XDG variable goes in only when it is absolute. The spec calls a
//     relative one invalid and adrg/xdg ignores it, falling back to the HOME
//     default, so pinning it would turn an override the install ignored into
//     one the timer obeys: the empty-database bug again, from the other side.
//   - VINTED_CONFIG is read raw, so a relative one does take effect here and
//     must be resolved: the scheduler runs the job from its own directory.
func serviceEnv() [][2]string {
	var env [][2]string
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, [2]string{"HOME", home})
	}
	for _, k := range serviceEnvXDGKeys {
		if v := os.Getenv(k); filepath.IsAbs(v) {
			env = append(env, [2]string{k, v})
		}
	}
	if v := os.Getenv("VINTED_CONFIG"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			v = abs
		}
		env = append(env, [2]string{"VINTED_CONFIG", v})
	}
	for _, k := range serviceEnvURLKeys {
		if v := os.Getenv(k); v != "" {
			env = append(env, [2]string{k, v})
		}
	}
	return env
}

// systemdUnits renders the service and timer bodies for one profile.
//
// OnBootSec makes the first run happen shortly after boot (and right away on
// install, since boot is long past); OnUnitActiveSec keeps the cadence from
// each run's end. Persistent= is for OnCalendar timers only.
//
// ExecStart is word-split, so the executable is quoted: a home directory with
// a space in it would otherwise produce a unit that fails on every trigger.
// The append: paths take the rest of the line and need no quoting, but every
// value still has its % doubled against specifier expansion.
func systemdUnits(profile, exe string, args []string, logFile string, iv time.Duration, env [][2]string) (service, timer string) {
	envLines := systemdEnv(env)
	log := systemdLiteral(logFile)
	service = fmt.Sprintf("[Unit]\nDescription=vinted-cli watch checks (%s)\n\n[Service]\nType=oneshot\n%sExecStart=%s %s\nStandardOutput=append:%s\nStandardError=append:%s\n",
		profile, envLines, systemdQuote(exe), systemdLiteral(strings.Join(args, " ")), log, log)
	timer = fmt.Sprintf("[Unit]\nDescription=vinted-cli watch timer (%s)\n\n[Timer]\nOnBootSec=2m\nOnUnitActiveSec=%s\n\n[Install]\nWantedBy=timers.target\n",
		profile, formatSystemd(iv))
	return service, timer
}

// launchdPlist renders the agent. Every value is XML-escaped: a path or URL
// carrying & or < would otherwise produce a plist launchd refuses to parse,
// and an agent that fails to parse simply never runs. StartInterval counts
// whole seconds from each run; RunAtLoad makes the first one happen at load
// rather than an interval later, which is also how the agent comes back after
// a reboot, since launchd loads ~/Library/LaunchAgents at login.
func launchdPlist(label, exe string, args []string, logFile string, iv time.Duration, env [][2]string) string {
	var progArgs strings.Builder
	for _, s := range append([]string{exe}, args...) {
		progArgs.WriteString("    <string>" + xmlEscape(s) + "</string>\n")
	}
	var envDict strings.Builder
	if len(env) > 0 {
		envDict.WriteString("  <key>EnvironmentVariables</key><dict>\n")
		for _, kv := range env {
			envDict.WriteString("    <key>" + xmlEscape(kv[0]) + "</key><string>" + xmlEscape(kv[1]) + "</string>\n")
		}
		envDict.WriteString("  </dict>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
%s  </array>
%s  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, xmlEscape(label), progArgs.String(), envDict.String(), int(iv.Seconds()), xmlEscape(logFile), xmlEscape(logFile))
}

// schtasksCreate is the command a Windows user runs by hand. schtasks counts
// whole minutes and rejects 0, so the interval rounds up to at least 1. The
// inner \" quotes keep a task command whose path has spaces in one argument,
// which "C:\Program Files\..." needs.
func schtasksCreate(taskName, exe string, args []string, iv time.Duration) string {
	return fmt.Sprintf(`schtasks /Create /SC MINUTE /MO %d /TN "%s" /TR "\"%s\" %s"`,
		max(1, int(math.Ceil(iv.Minutes()))), taskName, exe, strings.Join(args, " "))
}

func (a *App) serviceInstall(iv time.Duration) error {
	if err := checkServiceProxies(); err != nil {
		return err
	}
	exe, args, err := a.serviceCommandLine(iv)
	if err != nil {
		return err
	}
	u := a.serviceUnit()
	if u == nil {
		return a.printSchtasks(schtasksCreate(a.serviceUnitName(), exe, args, iv),
			"scheduled install is not automated on Windows; run the command above in cmd.exe")
	}
	logFile := a.serviceLogFile()
	// A scheduler shares no working directory with the install command, so
	// every path written into the unit has to be absolute already. These come
	// from $HOME, and a relative $HOME makes all of them relative: systemd
	// ignores a relative StandardOutput= and never looks where the units
	// landed. Refuse rather than install something that quietly does nothing.
	for _, p := range append([]string{logFile}, u.files...) {
		if !filepath.IsAbs(p) {
			return output.Usagef("%s is not an absolute path, so the scheduler could not use it. Set HOME to an absolute path and install again", p)
		}
	}
	if err := os.MkdirAll(a.Paths.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.files[0]), 0o755); err != nil {
		return err
	}
	view := serviceView{Platform: u.platform, Unit: u.name, Files: u.files, Log: logFile}
	switch u.platform {
	case "systemd":
		service, timer := systemdUnits(a.Profile, exe, args, logFile, iv, serviceEnv())
		if err := os.WriteFile(u.files[0], []byte(service), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(u.files[1], []byte(timer), 0o644); err != nil {
			return err
		}
		if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
		}
		if out, err := exec.Command("systemctl", "--user", "enable", "--now", u.name).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl enable: %v: %s", err, out)
		}
		// Without linger the user manager, and this timer with it, stops at
		// logout and does not start at boot until the next login.
		if !lingerEnabled() {
			if out, err := exec.Command("loginctl", "enable-linger").CombinedOutput(); err != nil {
				view.Hint = fmt.Sprintf("could not enable linger (%v: %s); run `loginctl enable-linger` so the timer survives logout and reboot", err, strings.TrimSpace(string(out)))
			}
		}
		view.State = "enabled"
	case "launchd":
		if err := os.WriteFile(u.files[0], []byte(launchdPlist(u.name, exe, args, logFile, iv, launchdEnv(serviceEnv()))), 0o644); err != nil {
			return err
		}
		_ = exec.Command("launchctl", "unload", u.files[0]).Run()
		if out, err := exec.Command("launchctl", "load", u.files[0]).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl load: %v: %s", err, out)
		}
		view.State = "loaded"
	}
	return a.Printer.Print(view)
}

// systemdLiteral escapes a value so systemd reads it as the text it is. A %
// starts a specifier expansion: an unknown one makes systemd drop the whole
// assignment, and a known one silently substitutes something else, so a path
// or a percent-encoded URL carrying % must double it.
func systemdLiteral(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// systemdQuote is systemdLiteral plus the double quotes the unit parser
// understands, so a path or a value with a space also survives word splitting.
func systemdQuote(s string) string {
	return `"` + systemdLiteral(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)) + `"`
}

// xmlEscape makes a string safe as plist text. encoding/xml's escaper turns
// newlines into entities, which is right but noisy; the five predefined
// entities are all a plist needs.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

func formatSystemd(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d.Minutes())) + "min"
	}
	return strconv.Itoa(int(d.Seconds())) + "s"
}

// printSchtasks is the Windows path: no integration, the user runs the printed
// command. Other platforms without a scheduler are refused outright rather
// than handed a cmd.exe command line.
func (a *App) printSchtasks(cmdline, hint string) error {
	if runtime.GOOS != "windows" {
		return output.Usagef("watch service has no scheduler integration on %s; use cron with `vinted watch check --all`", runtime.GOOS)
	}
	return a.Printer.Print(serviceView{Platform: "schtasks", Command: cmdline, Hint: hint})
}

// serviceUninstall stops the timer, then removes the units and the scheduler
// log, so nothing of the schedule outlives it. A scheduler that cannot be
// stopped is an error, not a "removed": deleting the files under a still
// loaded unit would leave it running blind. Watches and events stay in the
// state database.
func (a *App) serviceUninstall() error {
	u := a.serviceUnit()
	if u == nil {
		return a.printSchtasks(fmt.Sprintf(`schtasks /Delete /TN "%s" /F`, a.serviceUnitName()), "run the command above in cmd.exe")
	}
	if u.installed() {
		var stop *exec.Cmd
		switch u.platform {
		case "systemd":
			stop = exec.Command("systemctl", "--user", "disable", "--now", u.name)
		case "launchd":
			stop = exec.Command("launchctl", "unload", u.files[0])
		}
		if out, err := stop.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(stop.Args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	for _, f := range u.files {
		_ = os.Remove(f)
	}
	if u.platform == "systemd" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	log := a.serviceLogFile()
	_ = os.Remove(log)
	_ = os.Remove(log + ".1")
	return a.Printer.Print(serviceView{Platform: u.platform, Unit: u.name, State: "removed"})
}

func (a *App) serviceStatus() error {
	u := a.serviceUnit()
	if u == nil {
		return a.printSchtasks(fmt.Sprintf(`schtasks /Query /TN "%s"`, a.serviceUnitName()), "")
	}
	state, lastRun := u.state()
	return a.Printer.Print(serviceView{Platform: u.platform, Unit: u.name, State: state, LastRun: lastRun, Log: a.serviceLogFile()})
}

// sink commands

type sinkRow struct {
	Name string `json:"name"`
	config.SinkConfig
	Error string `json:"error,omitempty"`
}

type sinkList []sinkRow

func (l sinkList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, s := range l {
		target := s.URL
		if s.Type == "ntfy" {
			target = strings.TrimRight(s.URL, "/") + "/" + s.Topic
		}
		if s.Type == "exec" {
			target = strings.Join(s.Command, " ")
		}
		rows = append(rows, []string{s.Name, s.Type, output.Truncate(target, 50), s.Error})
	}
	output.Table(w, color, []string{"SINK", "TYPE", "TARGET", "PROBLEM"}, rows)
}

func (a *App) sinkCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sink", Short: "Notification targets declared in config"}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List configured sinks and validation problems", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				list := sinkList{}
				for name, s := range a.Cfg.Sinks {
					row := sinkRow{Name: name, SinkConfig: s}
					if err := sink.Validate(name, s); err != nil {
						row.Error = err.Error()
					}
					list = append(list, row)
				}
				for i := 1; i < len(list); i++ {
					for j := i; j > 0 && list[j].Name < list[j-1].Name; j-- {
						list[j], list[j-1] = list[j-1], list[j]
					}
				}
				return a.Printer.Print(list)
			},
		},
		&cobra.Command{
			Use: "test NAME", Short: "Send a synthetic event to a sink", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				s, ok := a.Cfg.Sinks[args[0]]
				if !ok {
					return output.Usagef("sink %q is not in config", args[0])
				}
				if err := sink.Validate(args[0], s); err != nil {
					return output.Usagef("%v", err)
				}
				item, _ := json.Marshal(vinted.Item{ID: 1, Title: "vinted-cli sink test", Price: 42, Currency: "EUR", URL: "https://github.com/Microck/vinted-cli"})
				ev := store.Event{Type: "item.new", Watch: "sink-test", Profile: a.Profile, At: time.Now().UTC(), Item: item}
				if err := sink.Deliver(cmd.Context(), args[0], s, ev); err != nil {
					return err
				}
				return a.Printer.Print(map[string]any{"sink": args[0], "delivered": true})
			},
		},
	)
	return cmd
}
