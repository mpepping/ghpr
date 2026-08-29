package githubapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v81/github"
)

// Explain turns a GitHub API error into a single line a user can act on.
// Rate limits, permission problems and missing resources all surface as opaque
// "403 Forbidden" style messages otherwise.
func Explain(err error) string {
	if err == nil {
		return ""
	}

	var rateLimit *github.RateLimitError
	if errors.As(err, &rateLimit) {
		wait := time.Until(rateLimit.Rate.Reset.Time).Round(time.Second)
		if wait < 0 {
			wait = 0
		}
		return fmt.Sprintf("GitHub API rate limit exceeded (%d/%d used); resets in %s",
			rateLimit.Rate.Limit-rateLimit.Rate.Remaining, rateLimit.Rate.Limit, wait)
	}

	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		if abuse.RetryAfter != nil {
			return fmt.Sprintf("GitHub secondary rate limit hit; retry in %s", abuse.RetryAfter.Round(time.Second))
		}
		return "GitHub secondary rate limit hit; slow down and retry"
	}

	var response *github.ErrorResponse
	if errors.As(err, &response) {
		switch response.Response.StatusCode {
		case http.StatusUnauthorized:
			return "GitHub rejected the token; run `gh auth login` or refresh GH_TOKEN"
		case http.StatusForbidden:
			return "not permitted: " + response.Message + " (the token may lack the required scope)"
		case http.StatusNotFound:
			return "not found: the repository may be private or the token may lack access"
		case http.StatusUnprocessableEntity:
			if len(response.Errors) > 0 {
				return response.Message + ": " + response.Errors[0].Message
			}
			return response.Message
		}
	}

	return err.Error()
}

// retryTransport retries requests that GitHub rejected because of a rate
// limit. GitHub tells us exactly how long to wait, so a bounded number of
// well-timed retries is far better than failing a whole batch.
type retryTransport struct {
	base       http.RoundTripper
	maxRetries int
	maxWait    time.Duration
	sleep      func(time.Duration) <-chan time.Time
}

func newRetryTransport(base http.RoundTripper) *retryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &retryTransport{base: base, maxRetries: 3, maxWait: 90 * time.Second, sleep: time.After}
}

func (t *retryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		response, err := t.base.RoundTrip(request)
		if err != nil || attempt >= t.maxRetries {
			return response, err
		}

		wait, retry := retryDelay(response)
		if !retry || wait > t.maxWait {
			return response, nil
		}
		// The body must be drained and replaced before the request is reused.
		drain(response)
		if request.GetBody != nil {
			body, bodyErr := request.GetBody()
			if bodyErr != nil {
				return response, nil
			}
			request.Body = body
		}

		select {
		case <-t.sleep(wait):
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
}

// retryDelay reports how long to wait before retrying, based on the headers
// GitHub sets for primary and secondary rate limits.
func retryDelay(response *http.Response) (time.Duration, bool) {
	if response == nil {
		return 0, false
	}
	if response.StatusCode != http.StatusForbidden && response.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}

	// Secondary rate limits use Retry-After (seconds).
	if value := response.Header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return time.Duration(seconds)*time.Second + time.Second, true
		}
	}

	// Primary rate limits report the reset as a Unix timestamp, but only when
	// the remaining quota is actually exhausted.
	if response.Header.Get("X-RateLimit-Remaining") == "0" {
		if value := response.Header.Get("X-RateLimit-Reset"); value != "" {
			if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
				wait := time.Until(time.Unix(unix, 0))
				if wait < 0 {
					wait = 0
				}
				return wait + time.Second, true
			}
		}
	}
	return 0, false
}

func drain(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_ = response.Body.Close()
}
