package githubapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v81/github"
)

const (
	maxSearchResults = 1000
	maxStateBatch    = 50
	searchPageSize   = 100
)

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

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
)

// Client performs the GitHub operations used by ghpr.
type Client struct {
	github *github.Client

	viewerMu    sync.Mutex
	viewerLogin string
}

// NewClient creates an authenticated client for github.com.
func NewClient(token string) *Client {
	return NewClientWithGitHub(github.NewClient(nil).WithAuthToken(token))
}

// NewClientWithGitHub wraps a go-github client. It is primarily useful for tests
// and callers that need to customize the HTTP transport or API base URL.
func NewClientWithGitHub(client *github.Client) *Client {
	return &Client{github: client}
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

// ListOpenPullRequests finds open pull requests in repositories owned by owner.
// GitHub's search API returns at most 1,000 results for any query.
func (c *Client) ListOpenPullRequests(ctx context.Context, owner string, limit int) ([]PullRequest, error) {
	if !ownerPattern.MatchString(owner) {
		return nil, fmt.Errorf("invalid GitHub owner %q", owner)
	}
	if limit <= 0 || limit > maxSearchResults {
		limit = maxSearchResults
	}

	qualifier, err := c.ownerQualifier(ctx, owner)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf("is:pr is:open %s:%s archived:false", qualifier, owner)
	pulls := make([]PullRequest, 0, min(limit, searchPageSize))
	seen := make(map[string]bool, min(limit, searchPageSize))

	// PerPage must stay constant across pages: GitHub derives the result offset
	// from (page-1)*per_page, so shrinking it on the last page would re-request
	// results that were already collected and skip the ones that follow.
	for page := 1; len(pulls) < limit; page++ {
		result, response, err := c.github.Search.Issues(ctx, query, &github.SearchOptions{
			Sort:        "updated",
			Order:       "desc",
			ListOptions: github.ListOptions{Page: page, PerPage: searchPageSize},
		})
		if err != nil {
			return nil, fmt.Errorf("search open pull requests for %s: %w", owner, err)
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

func (c *Client) ownerQualifier(ctx context.Context, owner string) (string, error) {
	account, _, err := c.github.Users.Get(ctx, owner)
	if err != nil {
		return "", fmt.Errorf("get GitHub owner %s: %w", owner, err)
	}
	if strings.EqualFold(account.GetType(), "Organization") {
		return "org", nil
	}
	return "user", nil
}

// Diff returns a pull request's unified diff.
func (c *Client) Diff(ctx context.Context, pr PullRequest) (string, error) {
	diff, _, err := c.github.PullRequests.GetRaw(ctx, pr.Owner, pr.Repo, pr.Number, github.RawOptions{Type: github.Diff})
	if err != nil {
		return "", fmt.Errorf("get diff for %s: %w", pr.Key(), err)
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
	if len(pulls) > maxStateBatch {
		return nil, fmt.Errorf("pull request state batch contains %d pull requests; maximum is %d", len(pulls), maxStateBatch)
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

	var response pullRequestStatesGraphQLResponse
	if _, err := c.github.Do(ctx, request, &response); err != nil {
		return nil, fmt.Errorf("get pull request states: %w", err)
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

// ApproveAndMerge approves a pull request, then tries to enable squash
// auto-merge. If auto-merge is unavailable, it falls back to a direct squash
// merge, matching the behavior of the reference shell script.
//
// Pull requests opened by the authenticated user are merged without an
// approving review, because GitHub does not allow approving your own work.
func (c *Client) ApproveAndMerge(ctx context.Context, pr PullRequest) (MergeOutcome, error) {
	if !c.authoredByViewer(ctx, pr) {
		_, _, err := c.github.PullRequests.CreateReview(ctx, pr.Owner, pr.Repo, pr.Number, &github.PullRequestReviewRequest{
			Event: github.Ptr("APPROVE"),
		})
		if err != nil {
			return "", fmt.Errorf("approve %s: %w", pr.Key(), err)
		}
	}

	autoOutcome, autoErr := c.enableAutoMerge(ctx, pr)
	if autoErr == nil {
		return autoOutcome, nil
	}

	result, _, mergeErr := c.github.PullRequests.Merge(ctx, pr.Owner, pr.Repo, pr.Number, "", &github.PullRequestOptions{
		MergeMethod: "squash",
	})
	if mergeErr != nil {
		return "", fmt.Errorf("enable auto-merge: %v; direct squash merge: %w", autoErr, mergeErr)
	}
	if !result.GetMerged() {
		return "", fmt.Errorf("enable auto-merge: %v; direct squash merge rejected: %s", autoErr, result.GetMessage())
	}
	return MergeOutcomeMerged, nil
}

// RequestChanges submits a pull request review that requests changes.
func (c *Client) RequestChanges(ctx context.Context, pr PullRequest, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("a reason is required when requesting changes")
	}
	if c.authoredByViewer(ctx, pr) {
		return fmt.Errorf("request changes on %s: GitHub does not allow reviewing your own pull request", pr.Key())
	}
	_, _, err := c.github.PullRequests.CreateReview(ctx, pr.Owner, pr.Repo, pr.Number, &github.PullRequestReviewRequest{
		Event: github.Ptr("REQUEST_CHANGES"),
		Body:  github.Ptr(body),
	})
	if err != nil {
		return fmt.Errorf("request changes on %s: %w", pr.Key(), err)
	}
	return nil
}

// Close closes a pull request, adding a comment first when body is not empty.
func (c *Client) Close(ctx context.Context, pr PullRequest, body string) error {
	body = strings.TrimSpace(body)
	if body != "" {
		_, _, err := c.github.Issues.CreateComment(ctx, pr.Owner, pr.Repo, pr.Number, &github.IssueComment{Body: github.Ptr(body)})
		if err != nil {
			return fmt.Errorf("comment on %s before closing: %w", pr.Key(), err)
		}
	}

	_, _, err := c.github.Issues.Edit(ctx, pr.Owner, pr.Repo, pr.Number, &github.IssueRequest{
		State: github.Ptr("closed"),
	})
	if err != nil {
		return fmt.Errorf("close %s: %w", pr.Key(), err)
	}
	return nil
}

func (c *Client) enableAutoMerge(ctx context.Context, pr PullRequest) (MergeOutcome, error) {
	fullPR, _, err := c.github.PullRequests.Get(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return "", fmt.Errorf("get pull request node ID: %w", err)
	}
	if fullPR.GetNodeID() == "" {
		return "", errors.New("GitHub returned a pull request without a node ID")
	}

	request := graphQLRequest{
		Query: `mutation EnableAutoMerge($pullRequestId: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $pullRequestId, mergeMethod: SQUASH}) {
    pullRequest { merged autoMergeRequest { enabledAt } }
  }
}`,
		Variables: map[string]any{"pullRequestId": fullPR.GetNodeID()},
	}
	httpRequest, err := c.github.NewRequest("POST", "graphql", request)
	if err != nil {
		return "", fmt.Errorf("create GraphQL request: %w", err)
	}

	var response graphQLResponse
	if _, err := c.github.Do(ctx, httpRequest, &response); err != nil {
		return "", err
	}
	if len(response.Errors) > 0 {
		messages := make([]string, 0, len(response.Errors))
		for _, graphQLError := range response.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return "", errors.New(strings.Join(messages, "; "))
	}
	if response.Data.EnableAutoMerge.PullRequest.Merged {
		return MergeOutcomeMerged, nil
	}
	return MergeOutcomeScheduled, nil
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
