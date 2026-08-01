package githubapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v81/github"
)

const (
	maxSearchResults    = 1000
	maxBuildStatusBatch = 50
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

// MergeOutcome describes what GitHub did after an approve-and-merge action.
type MergeOutcome string

const (
	MergeOutcomeMerged    MergeOutcome = "merged"
	MergeOutcomeScheduled MergeOutcome = "auto-merge enabled"
)

// Client performs the GitHub operations used by ghpr.
type Client struct {
	github *github.Client
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
	return user.GetLogin(), nil
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
	pulls := make([]PullRequest, 0, min(limit, 100))

	for page := 1; len(pulls) < limit; page++ {
		perPage := min(100, limit-len(pulls))
		result, response, err := c.github.Search.Issues(ctx, query, &github.SearchOptions{
			Sort:        "updated",
			Order:       "desc",
			ListOptions: github.ListOptions{Page: page, PerPage: perPage},
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
			pulls = append(pulls, PullRequest{
				Owner:     prOwner,
				Repo:      repo,
				Number:    issue.GetNumber(),
				Title:     issue.GetTitle(),
				URL:       issue.GetHTMLURL(),
				Author:    issue.GetUser().GetLogin(),
				Draft:     issue.GetDraft(),
				UpdatedAt: updatedAt,
			})
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

// BuildStatuses returns the aggregate status-check rollup for each pull
// request's latest commit. Requests are deliberately bounded so callers can
// load large lists in rate-friendly batches.
func (c *Client) BuildStatuses(ctx context.Context, pulls []PullRequest) (map[string]BuildStatus, error) {
	if len(pulls) == 0 {
		return map[string]BuildStatus{}, nil
	}
	if len(pulls) > maxBuildStatusBatch {
		return nil, fmt.Errorf("build status batch contains %d pull requests; maximum is %d", len(pulls), maxBuildStatusBatch)
	}

	declarations := make([]string, 0, len(pulls))
	selections := make([]string, 0, len(pulls))
	variables := make(map[string]any, len(pulls))
	statuses := make(map[string]BuildStatus, len(pulls))
	for index, pr := range pulls {
		variable := fmt.Sprintf("url%d", index)
		alias := fmt.Sprintf("pr%d", index)
		declarations = append(declarations, "$"+variable+": URI!")
		selections = append(selections, fmt.Sprintf(`%s: resource(url: $%s) {
  ... on PullRequest {
    commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
  }
}`, alias, variable))
		variables[variable] = pr.URL
		statuses[pr.Key()] = BuildStatusNone
	}

	requestBody := graphQLRequest{
		Query:     fmt.Sprintf("query BuildStatuses(%s) {\n%s\n}", strings.Join(declarations, ", "), strings.Join(selections, "\n")),
		Variables: variables,
	}
	request, err := c.github.NewRequest("POST", "graphql", requestBody)
	if err != nil {
		return nil, fmt.Errorf("create build status GraphQL request: %w", err)
	}

	var response buildStatusesGraphQLResponse
	if _, err := c.github.Do(ctx, request, &response); err != nil {
		return nil, fmt.Errorf("get pull request build statuses: %w", err)
	}

	// If there are GraphQL errors, we still process available data and return
	// partial results. This ensures that a single failing PR doesn't prevent
	// all others from getting status updates. Individual PR errors will result
	// in BuildStatusUnknown for those PRs.
	if len(response.Errors) > 0 {
		// Note: We don't return an error here. Instead, we continue processing
		// and return partial results. PRs affected by errors will remain at
		// their initial BuildStatusNone or be set to BuildStatusUnknown.
	}

	for index, pr := range pulls {
		resource, exists := response.Data[fmt.Sprintf("pr%d", index)]
		if !exists {
			// PR data not in response (e.g., due to GraphQL error)
			statuses[pr.Key()] = BuildStatusUnknown
			continue
		}
		if len(resource.Commits.Nodes) == 0 || resource.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
			// No commit status data available
			continue
		}
		switch strings.ToUpper(resource.Commits.Nodes[0].Commit.StatusCheckRollup.State) {
		case "SUCCESS":
			statuses[pr.Key()] = BuildStatusSuccess
		case "PENDING", "EXPECTED":
			statuses[pr.Key()] = BuildStatusPending
		case "FAILURE", "ERROR":
			statuses[pr.Key()] = BuildStatusFailure
		default:
			statuses[pr.Key()] = BuildStatusUnknown
		}
	}

	return statuses, nil
}

type buildStatusesGraphQLResponse struct {
	Data map[string]struct {
		Commits struct {
			Nodes []struct {
				Commit struct {
					StatusCheckRollup *struct {
						State string `json:"state"`
					} `json:"statusCheckRollup"`
				} `json:"commit"`
			} `json:"nodes"`
		} `json:"commits"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// ApproveAndMerge approves a pull request, then tries to enable squash
// auto-merge. If auto-merge is unavailable, it falls back to a direct squash
// merge, matching the behavior of the reference shell script.
func (c *Client) ApproveAndMerge(ctx context.Context, pr PullRequest) (MergeOutcome, error) {
	_, _, err := c.github.PullRequests.CreateReview(ctx, pr.Owner, pr.Repo, pr.Number, &github.PullRequestReviewRequest{
		Event: github.Ptr("APPROVE"),
	})
	if err != nil {
		return "", fmt.Errorf("approve %s: %w", pr.Key(), err)
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
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
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
