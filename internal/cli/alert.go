package cli

import (
	"fmt"
	"io"
	"strconv"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type alertList []vinted.SavedSearch

func (l alertList) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(l))
	for _, s := range l {
		kind := "recent"
		if s.Saved {
			kind = "saved"
		}
		rows = append(rows, []string{output.Dim(strconv.FormatInt(s.ID, 10), color), output.Truncate(s.Title, 28), output.Truncate(s.Subtitle, 48), fmt.Sprint(s.NewItems), kind})
	}
	output.Table(w, color, []string{"ID", "SEARCH", "FILTERS", "NEW", "TYPE"}, rows)
}

func (a *App) alertCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "alert", Short: "Vinted's saved searches (server-side alerts), read-only"}
	var all bool
	list := &cobra.Command{
		Use: "list", Short: "List the account's saved searches", Args: cobra.NoArgs,
		Long: `List the searches the account saved on Vinted (the ones it sends new-item
alerts for), with the query each one runs. --all adds the recent searches
the search box remembers. Turn one into a local watch with:

  vinted watch add search --from-alert ID --name NAME`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			alerts, err := a.Client.SavedSearches(cmd.Context(), all)
			if err != nil {
				return err
			}
			return a.Printer.Print(alertList(alerts))
		},
	}
	list.Flags().BoolVar(&all, "all", false, "include recent (unsaved) searches")
	cmd.AddCommand(list)
	return cmd
}

// savedSearchParams loads the query of saved search ref for --from-alert.
func (a *App) savedSearchParams(cmd *cobra.Command, ref string) (vinted.SearchParams, error) {
	if ref == "" {
		return vinted.SearchParams{}, output.Usagef("--from-alert needs a saved search id. See `vinted alert list`")
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return vinted.SearchParams{}, output.Usagef("--from-alert %q is not a saved search id. See `vinted alert list`", ref)
	}
	if err := a.requireSession(); err != nil {
		return vinted.SearchParams{}, err
	}
	saved, err := a.Client.SavedSearch(cmd.Context(), id)
	if err != nil {
		return vinted.SearchParams{}, err
	}
	return saved.Query, nil
}

// searchFlagsGiven reports whether any flag in the set was passed. The set
// shares its *Flag values with the command, whose parse marks them Changed.
func searchFlagsGiven(fs *pflag.FlagSet) bool {
	given := false
	fs.VisitAll(func(f *pflag.Flag) { given = given || f.Changed })
	return given
}
