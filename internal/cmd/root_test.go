package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionDoesNotRequireToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), []string{"--version"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteArgs() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "ghpr dev") {
		t.Fatalf("version output = %q", got)
	}
}

func TestHelpReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), []string{"--help"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteArgs() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "Usage: ghpr") || !strings.Contains(got, "GH_TOKEN") {
		t.Fatalf("help output = %q", got)
	}
}

func TestTokenIsRequired(t *testing.T) {
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("ExecuteArgs() error = %v, want missing token", err)
	}
}

func TestLimitValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), []string{"--limit", "1001"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "between 1 and 1000") {
		t.Fatalf("ExecuteArgs() error = %v, want limit error", err)
	}
}
