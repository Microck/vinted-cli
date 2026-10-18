package cli

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/config"
	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/sink"
	"github.com/Microck/vinted-cli/internal/throttle"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// config

func (a *App) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and write config.toml",
		Long: `Read and write config.toml. Keys are dotted paths.

Examples:
  vinted config path
  vinted config set watch.interval 30m
  vinted config set request_interval 3s
  vinted config set profiles.default.domain www.vinted.fr
  vinted config set sinks.phone.type ntfy
  vinted config set sinks.phone.url https://ntfy.sh
  vinted config set sinks.phone.topic vinted-deals
  vinted config set sinks.script.command '["/home/me/bin/on-event", "--verbose"]'
  vinted config get sinks.phone`,
	}
	cmd.AddCommand(
		&cobra.Command{Use: "path", Short: "Print the config file path", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			return a.Printer.Print(map[string]string{"config": a.Paths.ConfigFile, "credentials": a.Paths.CredentialsFile, "state": a.Paths.StateDB})
		}},
		&cobra.Command{Use: "list", Short: "Print the whole config", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			return a.Printer.Print(a.Cfg)
		}},
		&cobra.Command{Use: "get KEY", Short: "Print one key", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := a.configDoc()
			if err != nil {
				return err
			}
			v, ok := lookup(doc, strings.Split(args[0], "."))
			if !ok {
				return output.Usagef("config has no key %q", args[0])
			}
			return a.Printer.Print(v)
		}},
		&cobra.Command{Use: "set KEY VALUE", Short: "Set one key (exec commands use TOML string arrays)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			doc, err := a.configDoc()
			if err != nil {
				return err
			}
			path := strings.Split(args[0], ".")
			value := typed(args[1])
			// Command is the only slice in config.Config. Parse it by its
			// schema, not by a leading bracket in arbitrary string values.
			if len(path) == 3 && path[0] == "sinks" && path[2] == "command" {
				var array struct {
					Value []string `toml:"value"`
				}
				dec := toml.NewDecoder(strings.NewReader("value = " + args[1]))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&array); err != nil {
					return output.Usagef("%q expects an array of strings, for example '[\"/home/me/bin/on-event\", \"--verbose\"]'", args[0])
				}
				value = array.Value
			}
			assign(doc, path, value)
			raw, err := toml.Marshal(doc)
			if err != nil {
				return err
			}
			// Validate strictly before writing so a bad key never lands on disk.
			dec := toml.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			var cfg config.Config
			if err := dec.Decode(&cfg); err != nil {
				return output.Usagef("%q is not a valid config key: %v", args[0], err)
			}
			if gap, err := cfg.RequestGap(); err != nil {
				return output.Usagef("request_interval: %v", err)
			} else if gap != 0 && gap < throttle.MinInterval {
				return output.Usagef("request_interval %s is below the floor of %s", gap, throttle.MinInterval)
			}
			if cfg.Watch.Interval != "" {
				if _, err := configuredWatchInterval(0, cfg.Watch.Interval); err != nil {
					return err
				}
			}
			if cfg.Proxy != "" {
				u, err := vinted.ParseProxy(cfg.Proxy)
				if err != nil {
					return output.Usagef("proxy: %v", err)
				}
				if u.User != nil {
					return output.Usagef("proxy: config.toml is world-readable, so it must not hold proxy credentials. Pass a proxy with credentials through VINTED_PROXY instead")
				}
			}
			if len(path) >= 2 && path[0] == "sinks" {
				if err := sink.CheckValues(path[1], cfg.Sinks[path[1]]); err != nil {
					return output.Usagef("%v", err)
				}
			}
			a.Cfg = cfg
			if err := a.saveConfig(); err != nil {
				return err
			}
			return a.Printer.Print(map[string]any{args[0]: value})
		}},
	)
	return cmd
}

func (a *App) configDoc() (map[string]any, error) {
	doc := map[string]any{}
	raw, err := os.ReadFile(a.Paths.ConfigFile)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	}
	return doc, nil
}

func lookup(doc map[string]any, path []string) (any, bool) {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func assign(doc map[string]any, path []string, v any) {
	cur := doc
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

func typed(s string) any {
	if s == "true" || s == "false" {
		return s == "true"
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// doctor

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type doctorReport struct {
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

func (r doctorReport) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(r.Checks))
	for _, c := range r.Checks {
		mark := "ok"
		if !c.OK {
			mark = "FAIL"
		}
		rows = append(rows, []string{mark, c.Name, c.Detail})
	}
	output.Table(w, color, nil, rows)
}

func (a *App) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Check config, sinks, proxy, cooldown, site, session and schedule", Args: cobra.NoArgs,
		Long: `Run these checks, in order, and print one line each:

  config          config.toml loaded (shows its path)
  sink NAME       each configured sink is complete and valid
  proxy           the proxy setting parses (shown without credentials), or none
  cooldown        no cooldown after a block is running; shows the request interval
  site reachable  the profile's Vinted site answers (one request)
  session         the profile's session works and names the account (one request)
  schedule        the state of the watch timer (systemd on Linux, launchd on macOS)

The proxy check sends nothing; an unusable proxy shows up as a failed site
check. While a cooldown lasts the site and session checks are skipped (they
would be refused) and doctor exits 5. Any other failed check exits 1.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rep := doctorReport{OK: true}
			add := func(name string, err error, detail string) {
				c := doctorCheck{Name: name, OK: err == nil, Detail: detail}
				if err != nil {
					c.Detail = err.Error()
					rep.OK = false
				}
				rep.Checks = append(rep.Checks, c)
			}
			add("config", nil, a.Paths.ConfigFile)
			for name, s := range a.Cfg.Sinks {
				add("sink "+name, sink.Validate(name, s), s.Type)
			}
			// The proxy check never touches the network: an unusable proxy
			// fails every request, and the site check below shows that.
			switch {
			case a.Proxy != "":
				add("proxy", nil, a.Proxy+" (all vinted traffic, no direct fallback)")
			case os.Getenv("VINTED_PROXY") != "" || a.Cfg.Proxy != "":
				add("proxy", a.applyProxy(), "")
			default:
				add("proxy", nil, "none (direct connection)")
			}
			// The cooldown is checked first and, while it lasts, replaces the
			// network checks: sending them is exactly what it forbids.
			cooling := a.Gate.Check()
			if cd, err := a.Gate.Status(); err != nil {
				add("cooldown", err, "")
			} else if cd.Blocked {
				add("cooldown", fmt.Errorf("vinted blocked requests to %s; the cli sends nothing until %s (%s left). See `vinted cooldown`", cd.Site, cd.BlockedUntil.Local().Format("2006-01-02 15:04 MST"), cd.Remaining), "")
			} else {
				add("cooldown", nil, "none; request interval "+cd.RequestInterval)
			}
			if cooling == nil {
				if _, err := a.Client.Categories(ctx); err != nil {
					add("site reachable", err, "")
				} else {
					add("site reachable", nil, a.Client.Base)
				}
				if a.Session == nil {
					add("session", fmt.Errorf("profile %q has no session", a.Profile), "")
				} else if me, err := a.Client.Me(ctx); err != nil {
					add("session", err, "")
				} else {
					add("session", nil, "logged in as "+me.Login)
				}
			}
			if u := a.serviceUnit(); u != nil {
				state, _ := u.state()
				add("schedule", nil, state)
			}
			if err := a.Printer.Print(rep); err != nil {
				return err
			}
			if cooling != nil {
				return cooling
			}
			if !rep.OK {
				// A failed check is not a usage mistake: exit 1, as documented.
				return errors.New("doctor found problems")
			}
			return nil
		},
	}
}

// skills: embedded agent-facing docs

//go:embed skills/*.md
var skillFiles embed.FS

func (a *App) skillsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "skills", Short: "Embedded usage docs for humans and agents"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List available docs", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			names, err := skillNames()
			if err != nil {
				return err
			}
			return a.Printer.Print(names)
		}},
		&cobra.Command{Use: "get NAME", Short: "Print one doc as markdown", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := skillFiles.ReadFile("skills/" + args[0] + ".md")
			if err != nil {
				names, _ := skillNames()
				return output.Usagef("no skill %q. Available: %s", args[0], strings.Join(names, ", "))
			}
			_, err = a.Stdout.Write(raw)
			return err
		}},
	)
	return cmd
}

func skillNames() ([]string, error) {
	entries, err := fs.ReadDir(skillFiles, "skills")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names, nil
}
