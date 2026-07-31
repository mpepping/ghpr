package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/auth"
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

// ExecuteArgs runs ghpr with explicit arguments and output streams.
func ExecuteArgs(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("ghpr", flag.ContinueOnError)
	flagOutput := stderr
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			flagOutput = stdout
			break
		}
	}
	flags.SetOutput(flagOutput)

	owner := flags.String("owner", "", "GitHub user or organization that owns the repositories")
	limit := flags.Int("limit", 1000, "maximum number of open pull requests to load (1-1000)")
	showVersion := flags.Bool("version", false, "print version information and exit")
	flags.Usage = func() {
		fmt.Fprintln(flagOutput, "ghpr manages open pull requests from a multi-select terminal UI.")
		fmt.Fprintln(flagOutput)
		fmt.Fprintln(flagOutput, "Usage: ghpr [options]")
		fmt.Fprintln(flagOutput)
		fmt.Fprintln(flagOutput, "Authentication uses ~/.config/gh/hosts.yml when present, otherwise GH_TOKEN or GITHUB_TOKEN.")
		fmt.Fprintln(flagOutput)
		fmt.Fprintln(flagOutput, "Options:")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *showVersion {
		fmt.Fprintf(stdout, "ghpr %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("--limit must be between 1 and 1000")
	}

	token, err := auth.Token()
	if err != nil {
		return err
	}

	client := githubapi.NewClient(token)
	resolvedOwner := strings.TrimSpace(*owner)
	if resolvedOwner == "" {
		resolvedOwner, err = client.CurrentOwner(ctx)
		if err != nil {
			return err
		}
	}

	pulls, err := client.ListOpenPullRequests(ctx, resolvedOwner, *limit)
	if err != nil {
		return err
	}
	if len(pulls) == 0 {
		fmt.Fprintf(stdout, "No open pull requests found in repositories owned by %s.\n", resolvedOwner)
		return nil
	}

	model := tui.New(ctx, client, resolvedOwner, pulls)
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx), tea.WithOutput(stdout))
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("run terminal UI: %w", err)
	}
	return nil
}
