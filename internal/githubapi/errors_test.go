package githubapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v81/github"
)

func TestExplainRateLimit(t *testing.T) {
	t.Parallel()

	err := &github.RateLimitError{
		Rate: github.Rate{
			Limit:     5000,
			Remaining: 0,
			Reset:     github.Timestamp{Time: time.Now().Add(90 * time.Second)},
		},
		Message: "API rate limit exceeded",
	}
	got := Explain(fmt.Errorf("search: %w", err))
	if !strings.Contains(got, "rate limit exceeded") || !strings.Contains(got, "resets in") {
		t.Fatalf("Explain() = %q", got)
	}
	if !strings.Contains(got, "5000/5000") {
		t.Fatalf("Explain() should report the quota, got %q", got)
	}
}

func TestExplainSecondaryRateLimit(t *testing.T) {
	t.Parallel()

	retryAfter := 42 * time.Second
	got := Explain(&github.AbuseRateLimitError{RetryAfter: &retryAfter})
	if !strings.Contains(got, "secondary rate limit") || !strings.Contains(got, "42s") {
		t.Fatalf("Explain() = %q", got)
	}
}

func TestExplainHTTPStatuses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "rejected the token"},
		{http.StatusForbidden, "not permitted"},
		{http.StatusNotFound, "not found"},
	}
	for _, testCase := range cases {
		err := &github.ErrorResponse{
			Response: &http.Response{StatusCode: testCase.status},
			Message:  "boom",
		}
		if got := Explain(err); !strings.Contains(got, testCase.want) {
			t.Errorf("Explain(%d) = %q, want it to mention %q", testCase.status, got, testCase.want)
		}
	}

	if Explain(nil) != "" {
		t.Error("Explain(nil) must be empty")
	}
}

// A rate-limited request should be retried once the reset time passes rather
// than failing the whole batch.
func TestRetryTransportRetriesAfterRateLimitReset(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			writer.Header().Set("X-RateLimit-Remaining", "0")
			writer.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Second).Unix(), 10))
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		writeJSON(t, writer, map[string]any{"login": "mpepping"})
	}))
	defer server.Close()

	transport := newRetryTransport(nil)
	// Keep the test fast: the delay is computed, not actually slept through.
	transport.sleep = func(time.Duration) <-chan time.Time {
		channel := make(chan time.Time, 1)
		channel <- time.Now()
		return channel
	}

	client := github.NewClient(&http.Client{Transport: transport})
	baseURL, _ := client.BaseURL.Parse(server.URL + "/")
	client.BaseURL = baseURL

	user, _, err := client.Users.Get(context.Background(), "")
	if err != nil {
		t.Fatalf("request failed despite the retry: %v", err)
	}
	if user.GetLogin() != "mpepping" {
		t.Fatalf("login = %q", user.GetLogin())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestRetryTransportHonoursRetryAfter(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	var waited time.Duration
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			writer.Header().Set("Retry-After", "3")
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		writeJSON(t, writer, map[string]any{"login": "mpepping"})
	}))
	defer server.Close()

	transport := newRetryTransport(nil)
	transport.sleep = func(delay time.Duration) <-chan time.Time {
		waited = delay
		channel := make(chan time.Time, 1)
		channel <- time.Now()
		return channel
	}

	client := github.NewClient(&http.Client{Transport: transport})
	baseURL, _ := client.BaseURL.Parse(server.URL + "/")
	client.BaseURL = baseURL

	if _, _, err := client.Users.Get(context.Background(), ""); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if waited < 3*time.Second {
		t.Fatalf("waited %s, want at least the advertised 3s", waited)
	}
}

// Ordinary failures must not be retried.
func TestRetryTransportIgnoresOtherErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		http.Error(writer, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := github.NewClient(&http.Client{Transport: newRetryTransport(nil)})
	baseURL, _ := client.BaseURL.Parse(server.URL + "/")
	client.BaseURL = baseURL

	if _, _, err := client.Users.Get(context.Background(), ""); err == nil {
		t.Fatal("expected the 404 to surface")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want no retry for a 404", got)
	}
}

func TestRetryDelayRejectsUnrelatedResponses(t *testing.T) {
	t.Parallel()

	if _, retry := retryDelay(nil); retry {
		t.Error("a nil response must not be retried")
	}
	ok := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if _, retry := retryDelay(ok); retry {
		t.Error("a 200 must not be retried")
	}
	// A 403 that is not a rate limit (for example a permission error).
	forbidden := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	if _, retry := retryDelay(forbidden); retry {
		t.Error("a plain 403 must not be retried")
	}
}

func TestNewClientConfiguresEnterpriseHost(t *testing.T) {
	t.Parallel()

	client, err := New(Config{Token: "x", Host: "https://github.example.com/"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := client.github.BaseURL.String(); !strings.Contains(got, "github.example.com/api/v3/") {
		t.Fatalf("enterprise base URL = %q", got)
	}

	public, err := New(Config{Token: "x"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := public.github.BaseURL.String(); got != "https://api.github.com/" {
		t.Fatalf("public base URL = %q", got)
	}
	if public.MergeMethod() != MergeMethodSquash {
		t.Fatalf("default merge method = %q, want squash", public.MergeMethod())
	}
}
