package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/auth"
	"github.com/mpepping/ghpr/internal/config"
	"github.com/mpepping/ghpr/internal/githubapi"
	"github.com/mpepping/ghpr/internal/tui"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Execute runs ghpr with process arguments and standard streams.
func Execute(ctx context.Context) error {
	return ExecuteArgs(ctx, os.Args[1:], os.Stdout, os.Stderr)
}

type options struct {
	owner        string
	limit        int
	scope        string
	mergeMethod  string
	deleteBranch bool
	dryRun       bool
	host         string
	filter       string
	asJSON       bool
	noColor      bool
	showVersion  bool
}

// newFlagSet registers every command line flag against opts.
func newFlagSet(opts *options, flagOutput io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("ghpr", flag.ContinueOnError)
	flags.SetOutput(flagOutput)

	flags.StringVar(&opts.owner, "owner", "", "GitHub user or organization that owns the repositories")
	flags.IntVar(&opts.limit, "limit", 1000, "maximum number of open pull requests to load (1-1000)")
	flags.StringVar(&opts.scope, "scope", string(githubapi.ScopeOwned), "which pull requests to load: owned, review-requested, involved or authored")
	flags.StringVar(&opts.mergeMethod, "merge-method", string(githubapi.MergeMethodSquash), "merge strategy: squash, merge or rebase")
	flags.BoolVar(&opts.deleteBranch, "delete-branch", false, "delete the head branch after a direct merge")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "show what every action would do without changing anything on GitHub")
	flags.StringVar(&opts.host, "host", "", "GitHub host, for GitHub Enterprise Server (default github.com)")
	flags.StringVar(&opts.filter, "filter", "", "start the terminal UI with this filter applied")
	flags.BoolVar(&opts.asJSON, "json", false, "print the pull requests as JSON and exit instead of starting the UI")
	flags.BoolVar(&opts.noColor, "no-color", false, "disable colored output")
	flags.BoolVar(&opts.showVersion, "version", false, "print version information and exit")

	flags.Usage = func() {
		printUsage(flagOutput, flags)
	}
	return flags
}

func printUsage(flagOutput io.Writer, flags *flag.FlagSet) {
	fmt.Fprintln(flagOutput, "ghpr manages open pull requests from a multi-select terminal UI.")
	fmt.Fprintln(flagOutput)
	fmt.Fprintln(flagOutput, "Usage: ghpr [options]")
	fmt.Fprintln(flagOutput)
	fmt.Fprintln(flagOutput, "Authentication uses ~/.config/gh/hosts.yml when present, otherwise GH_TOKEN or GITHUB_TOKEN.")
	fmt.Fprintln(flagOutput, "Defaults may be set in ~/.config/ghpr/config.yml.")
	fmt.Fprintln(flagOutput)
	fmt.Fprintln(flagOutput, "Options:")
	flags.PrintDefaults()
}

// ExecuteArgs runs ghpr with explicit arguments and output streams.
func ExecuteArgs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flagOutput := stderr
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			flagOutput = stdout
			break
		}
	}

	var opts options
	flags := newFlagSet(&opts, flagOutput)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if opts.showVersion {
		fmt.Fprintf(stdout, "ghpr %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	settings, err := resolve(&opts, flags)
	if err != nil {
		return err
	}
	return run(ctx, settings, stdout)
}

// settings is the fully resolved configuration: defaults, then the
// configuration file, then command line flags.
type settings struct {
	owner        string
	limit        int
	scope        githubapi.Scope
	mergeMethod  githubapi.MergeMethod
	deleteBranch bool
	dryRun       bool
	host         string
	filter       string
	editor       string
	asJSON       bool
	noColor      bool
}

func resolve(opts *options, flags *flag.FlagSet) (settings, error) {
	file, path, err := config.Load()
	if err != nil {
		return settings{}, err
	}
	_ = path

	// Only fall back to the configuration file for flags the user did not set.
	provided := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	if !provided["owner"] && file.Owner != "" {
		opts.owner = file.Owner
	}
	if !provided["limit"] && file.Limit > 0 {
		opts.limit = file.Limit
	}
	if !provided["scope"] && file.Scope != "" {
		opts.scope = file.Scope
	}
	if !provided["merge-method"] && file.MergeMethod != "" {
		opts.mergeMethod = file.MergeMethod
	}
	if !provided["delete-branch"] && file.DeleteBranch {
		opts.deleteBranch = true
	}
	if !provided["host"] && file.Host != "" {
		opts.host = file.Host
	}
	if !provided["filter"] && file.Filter != "" {
		opts.filter = file.Filter
	}
	if !provided["no-color"] && file.NoColor {
		opts.noColor = true
	}
	if opts.host == "" {
		opts.host = os.Getenv("GH_HOST")
	}

	resolved := settings{
		owner:        strings.TrimSpace(opts.owner),
		limit:        opts.limit,
		deleteBranch: opts.deleteBranch,
		dryRun:       opts.dryRun,
		host:         strings.TrimSpace(opts.host),
		filter:       strings.TrimSpace(opts.filter),
		editor:       strings.TrimSpace(file.Editor),
		asJSON:       opts.asJSON,
		// NO_COLOR is the cross-tool convention for disabling color.
		noColor: opts.noColor || os.Getenv("NO_COLOR") != "",
	}

	if resolved.limit < 1 || resolved.limit > 1000 {
		return settings{}, fmt.Errorf("--limit must be between 1 and 1000 (got %d)", resolved.limit)
	}
	if resolved.scope, err = githubapi.ParseScope(opts.scope); err != nil {
		return settings{}, err
	}
	if resolved.mergeMethod, err = githubapi.ParseMergeMethod(opts.mergeMethod); err != nil {
		return settings{}, err
	}
	if resolved.owner != "" {
		if err := githubapi.ValidateOwner(resolved.owner); err != nil {
			return settings{}, err
		}
	}
	return resolved, nil
}

func run(ctx context.Context, resolved settings, stdout io.Writer) error {
	token, err := auth.TokenForHost(resolved.host)
	if err != nil {
		return err
	}

	client, err := githubapi.New(githubapi.Config{
		Token:        token,
		Host:         resolved.host,
		MergeMethod:  resolved.mergeMethod,
		DeleteBranch: resolved.deleteBranch,
		DryRun:       resolved.dryRun,
		Timeout:      githubapi.DefaultTimeout,
	})
	if err != nil {
		return err
	}

	// The owned scope needs an owner; the others default to the viewer.
	owner := resolved.owner
	if owner == "" && resolved.scope.RequiresOwner() {
		owner, err = client.CurrentOwner(ctx)
		if err != nil {
			return err
		}
	}

	search := githubapi.SearchOptions{Owner: owner, Scope: resolved.scope, Limit: resolved.limit}
	if resolved.asJSON {
		return printJSON(ctx, client, search, stdout)
	}

	if resolved.noColor {
		tui.DisableColor()
	}
	model := tui.New(tui.Options{
		Context:     ctx,
		Service:     client,
		Search:      search,
		MergeMethod: resolved.mergeMethod,
		DryRun:      resolved.dryRun,
		Filter:      resolved.filter,
		Editor:      resolved.editor,
	})
	program := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
		tea.WithContext(ctx),
		tea.WithOutput(stdout),
	)
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal UI: %w", err)
	}
	return nil
}

// jsonPullRequest is the stable shape of the --json output.
type jsonPullRequest struct {
	Repository string    `json:"repository"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	Author     string    `json:"author"`
	Draft      bool      `json:"draft"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Checks     string    `json:"checks"`
	Review     string    `json:"review,omitempty"`
	Mergeable  string    `json:"mergeable"`
	Blocked    bool      `json:"blocked"`
}

func printJSON(ctx context.Context, client *githubapi.Client, search githubapi.SearchOptions, stdout io.Writer) error {
	pulls, err := client.ListOpenPullRequests(ctx, search)
	if err != nil {
		return err
	}

	// Load merge readiness in rate-friendly batches; a partial failure still
	// produces usable output.
	states := make(map[string]githubapi.PullRequestState, len(pulls))
	var warning error
	for start := 0; start < len(pulls); start += githubapi.MaxStateBatch {
		end := min(len(pulls), start+githubapi.MaxStateBatch)
		batch, batchErr := client.PullRequestStates(ctx, pulls[start:end])
		for key, state := range batch {
			states[key] = state
		}
		if batchErr != nil {
			warning = batchErr
		}
	}

	output := make([]jsonPullRequest, 0, len(pulls))
	for _, pr := range pulls {
		state := states[pr.Key()]
		output = append(output, jsonPullRequest{
			Repository: pr.Repository(),
			Number:     pr.Number,
			Title:      pr.Title,
			URL:        pr.URL,
			Author:     pr.Author,
			Draft:      pr.Draft,
			UpdatedAt:  pr.UpdatedAt,
			Checks:     string(state.Build),
			Review:     string(state.Review),
			Mergeable:  string(state.Mergeable),
			Blocked:    state.Blocked(),
		})
	}

	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(output); err != nil {
		return fmt.Errorf("write JSON output: %w", err)
	}
	if warning != nil {
		return fmt.Errorf("some pull request states could not be read: %w", warning)
	}
	return nil
}
