package githubapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v81/github"
)

const (
	maxSearchResults = 1000
	// MaxStateBatch is the largest batch PullRequestStates accepts.
	MaxStateBatch  = 50
	searchPageSize = 100

	// DefaultTimeout bounds a single API call so a hung request cannot freeze
	// the UI indefinitely.
	DefaultTimeout = 30 * time.Second
	// diffTimeout is more generous: large diffs are slow to generate.
	diffTimeout = 3 * time.Minute
)

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// MergeMethod selects how GitHub combines the commits of a pull request.
type MergeMethod string

const (
	MergeMethodSquash MergeMethod = "squash"
	MergeMethodMerge  MergeMethod = "merge"
	MergeMethodRebase MergeMethod = "rebase"
)

// ParseMergeMethod validates a user supplied merge method.
func ParseMergeMethod(value string) (MergeMethod, error) {
	switch MergeMethod(strings.ToLower(strings.TrimSpace(value))) {
	case MergeMethodSquash:
		return MergeMethodSquash, nil
	case MergeMethodMerge:
		return MergeMethodMerge, nil
	case MergeMethodRebase:
		return MergeMethodRebase, nil
	default:
		return "", fmt.Errorf("invalid merge method %q: use squash, merge or rebase", value)
	}
}

// GraphQL returns the PullRequestMergeMethod enum value.
func (m MergeMethod) GraphQL() string {
	return strings.ToUpper(string(m))
}

// Label describes the method the way it is shown in prompts.
func (m MergeMethod) Label() string {
	switch m {
	case MergeMethodRebase:
		return "rebase-merge"
	case MergeMethodMerge:
		return "merge commit"
	default:
		return "squash-merge"
	}
}

// Scope selects which open pull requests are listed.
type Scope string

const (
	// ScopeOwned lists pull requests in repositories owned by the account.
	ScopeOwned Scope = "owned"
	// ScopeReviewRequested lists pull requests waiting for the viewer's review.
	ScopeReviewRequested Scope = "review-requested"
	// ScopeInvolved lists pull requests the viewer authored, commented on,
	// was assigned to or was mentioned in.
	ScopeInvolved Scope = "involved"
	// ScopeAuthored lists pull requests the viewer opened.
	ScopeAuthored Scope = "authored"
)

// ParseScope validates a user supplied scope.
func ParseScope(value string) (Scope, error) {
	switch Scope(strings.ToLower(strings.TrimSpace(value))) {
	case ScopeOwned:
		return ScopeOwned, nil
	case ScopeReviewRequested:
		return ScopeReviewRequested, nil
	case ScopeInvolved:
		return ScopeInvolved, nil
	case ScopeAuthored:
		return ScopeAuthored, nil
	default:
		return "", fmt.Errorf("invalid scope %q: use owned, review-requested, involved or authored", value)
	}
}

// RequiresOwner reports whether the scope is meaningless without an owner.
func (s Scope) RequiresOwner() bool {
	return s == "" || s == ScopeOwned
}

// SearchOptions describes which pull requests to load.
type SearchOptions struct {
	Owner string
	Scope Scope
	Limit int
}

// ValidateOwner checks if the given owner string is a valid GitHub username or organization name.
// It returns an error if the owner is invalid.
func ValidateOwner(owner string) error {
	if !ownerPattern.MatchString(owner) {
		return fmt.Errorf("invalid GitHub owner %q: must contain only alphanumeric characters and hyphens, and be 1-39 characters long", owner)
	}
	return nil
}

// PullRequest contains the fields ghpr needs to display and update a pull request.
type PullRequest struct {
	Owner     string
	Repo      string
	Number    int
	Title     string
	URL       string
	Author    string
	Draft     bool
	UpdatedAt time.Time
}

// Key uniquely identifies a pull request.
func (p PullRequest) Key() string {
	return fmt.Sprintf("%s/%s#%d", p.Owner, p.Repo, p.Number)
}

// Repository returns the owner/name form of a repository.
func (p PullRequest) Repository() string {
	return p.Owner + "/" + p.Repo
}

// BuildStatus is the aggregate state of the checks and commit statuses for a
// pull request's latest commit.
type BuildStatus string

const (
	BuildStatusNone    BuildStatus = "none"
	BuildStatusPending BuildStatus = "pending"
	BuildStatusSuccess BuildStatus = "success"
	BuildStatusFailure BuildStatus = "failure"
	BuildStatusUnknown BuildStatus = "unknown"
)

// MergeableState reports whether a pull request can be merged into its base
// branch without conflicts.
type MergeableState string

const (
	MergeableUnknown     MergeableState = "unknown"
	MergeableClean       MergeableState = "mergeable"
	MergeableConflicting MergeableState = "conflicting"
)

// ReviewDecision is the overall review verdict GitHub computed for a pull
// request, taking branch protection rules into account.
type ReviewDecision string

const (
	ReviewDecisionNone             ReviewDecision = ""
	ReviewDecisionApproved         ReviewDecision = "approved"
	ReviewDecisionChangesRequested ReviewDecision = "changes requested"
	ReviewDecisionReviewRequired   ReviewDecision = "review required"
)

// PullRequestState bundles the signals that decide whether a pull request is
// ready to merge.
type PullRequestState struct {
	Build     BuildStatus
	Mergeable MergeableState
	Review    ReviewDecision
}

// Summary renders the state as a short human readable line.
func (s PullRequestState) Summary() string {
	build := string(s.Build)
	if build == "" {
		build = string(BuildStatusNone)
	}
	parts := []string{"CI: " + build}
	if s.Review != ReviewDecisionNone {
		parts = append(parts, "review: "+string(s.Review))
	}
	if s.Mergeable == MergeableConflicting {
		parts = append(parts, "conflicts")
	}
	return strings.Join(parts, " · ")
}

// Blocked reports whether merging this pull request is likely to fail.
func (s PullRequestState) Blocked() bool {
	return s.Mergeable == MergeableConflicting ||
		s.Review == ReviewDecisionChangesRequested ||
		s.Build == BuildStatusFailure
}

// MergeOutcome describes what GitHub did after an approve-and-merge action.
type MergeOutcome string

const (
	MergeOutcomeMerged    MergeOutcome = "merged"
	MergeOutcomeScheduled MergeOutcome = "auto-merge enabled"
	MergeOutcomeDryRun    MergeOutcome = "dry run"
)

// Config describes how the client talks to GitHub.
type Config struct {
	Token string
	// Host is the GitHub host. Empty or "github.com" uses the public API,
	// anything else is treated as a GitHub Enterprise Server instance.
	Host         string
	MergeMethod  MergeMethod
	DeleteBranch bool
	// DryRun makes every state-changing call a no-op that reports success.
	DryRun  bool
	Timeout time.Duration
}

// Client performs the GitHub operations used by ghpr.
type Client struct {
	github       *github.Client
	mergeMethod  MergeMethod
	deleteBranch bool
	dryRun       bool
	timeout      time.Duration

	viewerMu    sync.Mutex
	viewerLogin string
}

// Option customizes a Client.
type Option func(*Client)

// WithMergeMethod selects the merge strategy used by ApproveAndMerge.
func WithMergeMethod(method MergeMethod) Option {
	return func(c *Client) {
		if method != "" {
			c.mergeMethod = method
		}
	}
}

// WithDeleteBranch deletes the head branch after a direct merge.
func WithDeleteBranch(enabled bool) Option {
	return func(c *Client) { c.deleteBranch = enabled }
}

// WithDryRun turns every state-changing call into a no-op.
func WithDryRun(enabled bool) Option {
	return func(c *Client) { c.dryRun = enabled }
}

// WithTimeout bounds the duration of a single API call.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.timeout = timeout
		}
	}
}

// New creates an authenticated client from cfg.
func New(cfg Config) (*Client, error) {
	httpClient := &http.Client{Transport: newRetryTransport(nil)}
	client := github.NewClient(httpClient).WithAuthToken(cfg.Token)

	if host := normalizeHost(cfg.Host); host != "" && host != defaultHost {
		enterprise, err := client.WithEnterpriseURLs("https://"+host, "https://"+host)
		if err != nil {
			return nil, fmt.Errorf("configure GitHub Enterprise host %q: %w", cfg.Host, err)
		}
		client = enterprise
	}

	return NewClientWithGitHub(client,
		WithMergeMethod(cfg.MergeMethod),
		WithDeleteBranch(cfg.DeleteBranch),
		WithDryRun(cfg.DryRun),
		WithTimeout(cfg.Timeout),
	), nil
}

// NewClientWithGitHub wraps a go-github client. It is primarily useful for tests
// and callers that need to customize the HTTP transport or API base URL.
func NewClientWithGitHub(client *github.Client, options ...Option) *Client {
	wrapped := &Client{
		github:      client,
		mergeMethod: MergeMethodSquash,
		timeout:     DefaultTimeout,
	}
	for _, option := range options {
		option(wrapped)
	}
	return wrapped
}

const defaultHost = "github.com"

// normalizeHost reduces a host such as "https://github.example.com/" to its
// bare hostname.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	return strings.ToLower(strings.Trim(host, "/"))
}

// MergeMethod reports the configured merge strategy.
func (c *Client) MergeMethod() MergeMethod { return c.mergeMethod }

// DryRun reports whether state-changing calls are suppressed.
func (c *Client) DryRun() bool { return c.dryRun }

// withTimeout bounds a single API call. Callers that already have a deadline
// keep the earlier of the two.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// CurrentOwner returns the login belonging to the authenticated token.
func (c *Client) CurrentOwner(ctx context.Context) (string, error) {
	user, _, err := c.github.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get authenticated GitHub user: %w", err)
	}
	if user.GetLogin() == "" {
		return "", errors.New("GitHub returned an authenticated user without a login")
	}
	c.viewerMu.Lock()
	c.viewerLogin = user.GetLogin()
	c.viewerMu.Unlock()
	return user.GetLogin(), nil
}

// viewer returns the authenticated login, caching it after the first lookup.
func (c *Client) viewer(ctx context.Context) (string, error) {
	c.viewerMu.Lock()
	login := c.viewerLogin
	c.viewerMu.Unlock()
	if login != "" {
		return login, nil
	}
	return c.CurrentOwner(ctx)
}

// authoredByViewer reports whether the authenticated user opened the pull
// request. GitHub rejects self-reviews with 422 Unprocessable Entity, so those
// review requests must be skipped. Lookup failures are treated as "not the
// viewer" so the regular API call still runs and reports the real error.
func (c *Client) authoredByViewer(ctx context.Context, pr PullRequest) bool {
	if pr.Author == "" {
		return false
	}
	login, err := c.viewer(ctx)
	if err != nil {
		return false
	}
	return strings.EqualFold(login, pr.Author)
}

// ListOpenPullRequests finds open pull requests matching options.
// GitHub's search API returns at most 1,000 results for any query.
func (c *Client) ListOpenPullRequests(ctx context.Context, options SearchOptions) ([]PullRequest, error) {
	limit := options.Limit
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}

	query, err := c.buildQuery(ctx, options)
	if err != nil {
		return nil, err
	}
	pulls := make([]PullRequest, 0, min(limit, searchPageSize))
	seen := make(map[string]bool, min(limit, searchPageSize))

	// PerPage must stay constant across pages: GitHub derives the result offset
	// from (page-1)*per_page, so shrinking it on the last page would re-request
	// results that were already collected and skip the ones that follow.
	for page := 1; len(pulls) < limit; page++ {
		pageCtx, cancel := c.withTimeout(ctx)
		result, response, err := c.github.Search.Issues(pageCtx, query, &github.SearchOptions{
			Sort:        "updated",
			Order:       "desc",
			ListOptions: github.ListOptions{Page: page, PerPage: searchPageSize},
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("search open pull requests: %s", Explain(err))
		}

		for _, issue := range result.Issues {
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			prOwner, repo, err := repositoryFromURL(issue.GetRepositoryURL())
			if err != nil {
				return nil, fmt.Errorf("parse repository for pull request #%d: %w", issue.GetNumber(), err)
			}

			var updatedAt time.Time
			if issue.UpdatedAt != nil {
				updatedAt = issue.UpdatedAt.Time
			}
			pull := PullRequest{
				Owner:     prOwner,
				Repo:      repo,
				Number:    issue.GetNumber(),
				Title:     issue.GetTitle(),
				URL:       issue.GetHTMLURL(),
				Author:    issue.GetUser().GetLogin(),
				Draft:     issue.GetDraft(),
				UpdatedAt: updatedAt,
			}
			if seen[pull.Key()] {
				continue
			}
			seen[pull.Key()] = true
			pulls = append(pulls, pull)
			if len(pulls) == limit {
				break
			}
		}

		if response == nil || response.NextPage == 0 || len(result.Issues) == 0 {
			break
		}
	}

	return pulls, nil
}

// buildQuery turns the scope and owner into a GitHub search query.
func (c *Client) buildQuery(ctx context.Context, options SearchOptions) (string, error) {
	scope := options.Scope
	if scope == "" {
		scope = ScopeOwned
	}
	owner := strings.TrimSpace(options.Owner)
	if owner == "" && scope.RequiresOwner() {
		return "", errors.New("an owner is required for the owned scope")
	}
	if owner != "" && !ownerPattern.MatchString(owner) {
		return "", fmt.Errorf("invalid GitHub owner %q", owner)
	}

	terms := []string{"is:pr", "is:open"}
	switch scope {
	case ScopeReviewRequested:
		terms = append(terms, "review-requested:@me")
	case ScopeInvolved:
		terms = append(terms, "involves:@me")
	case ScopeAuthored:
		terms = append(terms, "author:@me")
	}

	// The owner narrows every scope, but only the owned scope needs to know
	// whether it is a user or an organization.
	if owner != "" {
		qualifier, err := c.ownerQualifier(ctx, owner)
		if err != nil {
			return "", err
		}
		terms = append(terms, qualifier+":"+owner)
	}
	terms = append(terms, "archived:false")
	return strings.Join(terms, " "), nil
}

func (c *Client) ownerQualifier(ctx context.Context, owner string) (string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	account, _, err := c.github.Users.Get(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("get GitHub owner %s: %s", owner, Explain(err))
	}
	if strings.EqualFold(account.GetType(), "Organization") {
		return "org", nil
	}
	return "user", nil
}

// Diff returns a pull request's unified diff.
func (c *Client) Diff(ctx context.Context, pr PullRequest) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, diffTimeout)
	defer cancel()

	diff, _, err := c.github.PullRequests.GetRaw(ctx, pr.Owner, pr.Repo, pr.Number, github.RawOptions{Type: github.Diff})
	if err != nil {
		return "", fmt.Errorf("get diff for %s: %s", pr.Key(), Explain(err))
	}
	return diff, nil
}

// PullRequestStates returns the merge readiness signals (checks rollup, merge
// conflicts and review decision) for each pull request. Requests are
// deliberately bounded so callers can load large lists in rate-friendly
// batches.
//
// GraphQL reports per-field errors while still returning data for the other
// aliases in the same query. PullRequestStates therefore always returns a
// usable map: pull requests covered by an error are marked unknown, and the
// combined error messages are returned so callers can surface them without
// discarding the results that did succeed.
func (c *Client) PullRequestStates(ctx context.Context, pulls []PullRequest) (map[string]PullRequestState, error) {
	if len(pulls) == 0 {
		return map[string]PullRequestState{}, nil
	}
	if len(pulls) > MaxStateBatch {
		return nil, fmt.Errorf("pull request state batch contains %d pull requests; maximum is %d", len(pulls), MaxStateBatch)
	}

	declarations := make([]string, 0, len(pulls))
	selections := make([]string, 0, len(pulls))
	variables := make(map[string]any, len(pulls))
	states := make(map[string]PullRequestState, len(pulls))
	for index, pr := range pulls {
		variable := fmt.Sprintf("url%d", index)
		alias := fmt.Sprintf("pr%d", index)
		declarations = append(declarations, "$"+variable+": URI!")
		selections = append(selections, fmt.Sprintf(`%s: resource(url: $%s) {
  ... on PullRequest {
    mergeable
    reviewDecision
    commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
  }
}`, alias, variable))
		variables[variable] = pr.URL
		states[pr.Key()] = PullRequestState{Build: BuildStatusNone, Mergeable: MergeableUnknown}
	}

	requestBody := graphQLRequest{
		Query:     fmt.Sprintf("query PullRequestStates(%s) {\n%s\n}", strings.Join(declarations, ", "), strings.Join(selections, "\n")),
		Variables: variables,
	}
	request, err := c.github.NewRequest("POST", "graphql", requestBody)
	if err != nil {
		return nil, fmt.Errorf("create pull request state GraphQL request: %w", err)
	}

	stateCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	var response pullRequestStatesGraphQLResponse
	if _, err := c.github.Do(stateCtx, request, &response); err != nil {
		return nil, fmt.Errorf("get pull request states: %s", Explain(err))
	}

	// Map each error back to the alias it belongs to so unaffected pull
	// requests keep their real state.
	failedAliases := make(map[string]bool, len(response.Errors))
	messages := make([]string, 0, len(response.Errors))
	for _, graphQLError := range response.Errors {
		if alias := aliasFromPath(graphQLError.Path); alias != "" {
			failedAliases[alias] = true
		}
		messages = append(messages, graphQLError.Message)
	}

	for index, pr := range pulls {
		alias := fmt.Sprintf("pr%d", index)
		resource, exists := response.Data[alias]
		if failedAliases[alias] || !exists || resource == nil {
			states[pr.Key()] = PullRequestState{Build: BuildStatusUnknown, Mergeable: MergeableUnknown}
			continue
		}

		state := PullRequestState{
			Build:     BuildStatusNone,
			Mergeable: mergeableState(resource.Mergeable),
			Review:    reviewDecision(resource.ReviewDecision),
		}
		if len(resource.Commits.Nodes) > 0 && resource.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			state.Build = buildStatus(resource.Commits.Nodes[0].Commit.StatusCheckRollup.State)
		}
		states[pr.Key()] = state
	}

	if len(messages) > 0 {
		return states, fmt.Errorf("pull request state query: %s", strings.Join(dedupe(messages), "; "))
	}
	return states, nil
}

func buildStatus(state string) BuildStatus {
	switch strings.ToUpper(state) {
	case "SUCCESS":
		return BuildStatusSuccess
	case "PENDING", "EXPECTED":
		return BuildStatusPending
	case "FAILURE", "ERROR":
		return BuildStatusFailure
	default:
		return BuildStatusUnknown
	}
}

func mergeableState(state string) MergeableState {
	switch strings.ToUpper(state) {
	case "MERGEABLE":
		return MergeableClean
	case "CONFLICTING":
		return MergeableConflicting
	default:
		// UNKNOWN means GitHub is still computing the merge commit.
		return MergeableUnknown
	}
}

func reviewDecision(decision string) ReviewDecision {
	switch strings.ToUpper(decision) {
	case "APPROVED":
		return ReviewDecisionApproved
	case "CHANGES_REQUESTED":
		return ReviewDecisionChangesRequested
	case "REVIEW_REQUIRED":
		return ReviewDecisionReviewRequired
	default:
		// Repositories without required reviews report an empty decision.
		return ReviewDecisionNone
	}
}

// aliasFromPath extracts the query alias ("pr3") a GraphQL error refers to.
func aliasFromPath(path []any) string {
	if len(path) == 0 {
		return ""
	}
	segment, ok := path[0].(string)
	if !ok || !aliasPattern.MatchString(segment) {
		return ""
	}
	return segment
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

var aliasPattern = regexp.MustCompile(`^pr\d+$`)

type pullRequestStatesGraphQLResponse struct {
	Data map[string]*struct {
		Mergeable      string `json:"mergeable"`
		ReviewDecision string `json:"reviewDecision"`
		Commits        struct {
			Nodes []struct {
				Commit struct {
					StatusCheckRollup *struct {
						State string `json:"state"`
					} `json:"statusCheckRollup"`
				} `json:"commit"`
			} `json:"nodes"`
		} `json:"commits"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphQLResponse struct {
	Data struct {
		EnableAutoMerge struct {
			PullRequest struct {
				Merged bool `json:"merged"`
			} `json:"pullRequest"`
		} `json:"enablePullRequestAutoMerge"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

type graphQLError struct {
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

func repositoryFromURL(rawURL string) (string, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for index := range parts {
		if parts[index] == "repos" && index+2 < len(parts) {
			return parts[index+1], parts[index+2], nil
		}
	}
	return "", "", fmt.Errorf("unexpected repository URL %q", rawURL)
}
