package githubapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v81/github"
)

// Approve submits an approving review without merging.
func (c *Client) Approve(ctx context.Context, pr PullRequest) error {
	if c.dryRun {
		return nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if c.authoredByViewer(ctx, pr) {
		return fmt.Errorf("approve %s: GitHub does not allow approving your own pull request", pr.Key())
	}
	if err := c.approve(ctx, pr); err != nil {
		return err
	}
	return nil
}

func (c *Client) approve(ctx context.Context, pr PullRequest) error {
	_, _, err := c.github.PullRequests.CreateReview(ctx, pr.Owner, pr.Repo, pr.Number, &github.PullRequestReviewRequest{
		Event: github.Ptr("APPROVE"),
	})
	if err != nil {
		return fmt.Errorf("approve %s: %s", pr.Key(), Explain(err))
	}
	return nil
}

// ApproveAndMerge approves a pull request, then tries to enable auto-merge
// using the configured merge method. If auto-merge is unavailable, it falls
// back to a direct merge.
//
// Pull requests opened by the authenticated user are merged without an
// approving review, because GitHub does not allow approving your own work.
func (c *Client) ApproveAndMerge(ctx context.Context, pr PullRequest) (MergeOutcome, error) {
	if c.dryRun {
		return MergeOutcomeDryRun, nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if !c.authoredByViewer(ctx, pr) {
		if err := c.approve(ctx, pr); err != nil {
			return "", err
		}
	}

	autoOutcome, autoErr := c.enableAutoMerge(ctx, pr)
	if autoErr == nil {
		return autoOutcome, nil
	}

	result, _, mergeErr := c.github.PullRequests.Merge(ctx, pr.Owner, pr.Repo, pr.Number, "", &github.PullRequestOptions{
		MergeMethod: string(c.mergeMethod),
	})
	if mergeErr != nil {
		return "", fmt.Errorf("enable auto-merge: %v; direct %s merge: %s", autoErr, c.mergeMethod, Explain(mergeErr))
	}
	if !result.GetMerged() {
		return "", fmt.Errorf("enable auto-merge: %v; direct %s merge rejected: %s", autoErr, c.mergeMethod, result.GetMessage())
	}

	// Only a direct merge can delete the branch here: with auto-merge GitHub
	// performs the merge later, and honours the repository's own
	// "automatically delete head branches" setting.
	if c.deleteBranch {
		if err := c.deleteHeadBranch(ctx, pr); err != nil {
			return MergeOutcomeMerged, fmt.Errorf("merged %s but could not delete the branch: %s", pr.Key(), Explain(err))
		}
	}
	return MergeOutcomeMerged, nil
}

func (c *Client) deleteHeadBranch(ctx context.Context, pr PullRequest) error {
	full, _, err := c.github.PullRequests.Get(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	head := full.GetHead()
	// Never touch a branch that lives in a fork.
	if head.GetRepo().GetFullName() != pr.Repository() {
		return errors.New("head branch belongs to a fork")
	}
	branch := head.GetRef()
	if branch == "" {
		return errors.New("pull request has no head branch")
	}
	_, err = c.github.Git.DeleteRef(ctx, pr.Owner, pr.Repo, "heads/"+branch)
	return err
}

// RequestChanges submits a pull request review that requests changes.
func (c *Client) RequestChanges(ctx context.Context, pr PullRequest, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("a reason is required when requesting changes")
	}
	if c.dryRun {
		return nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if c.authoredByViewer(ctx, pr) {
		return fmt.Errorf("request changes on %s: GitHub does not allow reviewing your own pull request", pr.Key())
	}
	_, _, err := c.github.PullRequests.CreateReview(ctx, pr.Owner, pr.Repo, pr.Number, &github.PullRequestReviewRequest{
		Event: github.Ptr("REQUEST_CHANGES"),
		Body:  github.Ptr(body),
	})
	if err != nil {
		return fmt.Errorf("request changes on %s: %s", pr.Key(), Explain(err))
	}
	return nil
}

// Comment adds a comment to a pull request without changing its state.
func (c *Client) Comment(ctx context.Context, pr PullRequest, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("a comment body is required")
	}
	if c.dryRun {
		return nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if err := c.comment(ctx, pr, body); err != nil {
		return err
	}
	return nil
}

func (c *Client) comment(ctx context.Context, pr PullRequest, body string) error {
	_, _, err := c.github.Issues.CreateComment(ctx, pr.Owner, pr.Repo, pr.Number, &github.IssueComment{Body: github.Ptr(body)})
	if err != nil {
		return fmt.Errorf("comment on %s: %s", pr.Key(), Explain(err))
	}
	return nil
}

// UpdateBranch merges the base branch into the pull request branch, which is
// what GitHub's "Update branch" button does.
func (c *Client) UpdateBranch(ctx context.Context, pr PullRequest) error {
	if c.dryRun {
		return nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	_, _, err := c.github.PullRequests.UpdateBranch(ctx, pr.Owner, pr.Repo, pr.Number, nil)
	if err != nil {
		// GitHub answers 202 Accepted, which go-github reports as an
		// AcceptedError even though the request succeeded.
		var accepted *github.AcceptedError
		if errors.As(err, &accepted) {
			return nil
		}
		return fmt.Errorf("update branch of %s: %s", pr.Key(), Explain(err))
	}
	return nil
}

// Close closes a pull request, adding a comment first when body is not empty.
func (c *Client) Close(ctx context.Context, pr PullRequest, body string) error {
	body = strings.TrimSpace(body)
	if c.dryRun {
		return nil
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if body != "" {
		if _, _, err := c.github.Issues.CreateComment(ctx, pr.Owner, pr.Repo, pr.Number, &github.IssueComment{Body: github.Ptr(body)}); err != nil {
			return fmt.Errorf("comment on %s before closing: %s", pr.Key(), Explain(err))
		}
	}

	_, _, err := c.github.Issues.Edit(ctx, pr.Owner, pr.Repo, pr.Number, &github.IssueRequest{
		State: github.Ptr("closed"),
	})
	if err != nil {
		return fmt.Errorf("close %s: %s", pr.Key(), Explain(err))
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
		Query: `mutation EnableAutoMerge($pullRequestId: ID!, $mergeMethod: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $pullRequestId, mergeMethod: $mergeMethod}) {
    pullRequest { merged autoMergeRequest { enabledAt } }
  }
}`,
		Variables: map[string]any{
			"pullRequestId": fullPR.GetNodeID(),
			"mergeMethod":   c.mergeMethod.GraphQL(),
		},
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
