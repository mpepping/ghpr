package browser

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestOpenUsesPlatformLauncher(t *testing.T) {
	t.Parallel()

	const target = "https://github.com/acme/widgets/pull/42"
	tests := []struct {
		goos     string
		wantName string
		wantArgs []string
	}{
		{goos: "darwin", wantName: "open", wantArgs: []string{target}},
		{goos: "linux", wantName: "xdg-open", wantArgs: []string{target}},
		{goos: "windows", wantName: "rundll32", wantArgs: []string{"url.dll,FileProtocolHandler", target}},
	}

	for _, test := range tests {
		t.Run(test.goos, func(t *testing.T) {
			t.Parallel()
			var gotName string
			var gotArgs []string
			err := open(target, test.goos, func(name string, args ...string) error {
				gotName = name
				gotArgs = args
				return nil
			})
			if err != nil {
				t.Fatalf("open() error = %v", err)
			}
			if gotName != test.wantName || !reflect.DeepEqual(gotArgs, test.wantArgs) {
				t.Fatalf("launcher = %q %v, want %q %v", gotName, gotArgs, test.wantName, test.wantArgs)
			}
		})
	}
}

func TestOpenRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	called := false
	err := open("javascript:alert(1)", "linux", func(string, ...string) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid HTTP URL") {
		t.Fatalf("open() error = %v, want invalid URL error", err)
	}
	if called {
		t.Fatal("launcher was called for an invalid URL")
	}
}

func TestOpenReportsUnsupportedPlatformAndLauncherFailure(t *testing.T) {
	t.Parallel()

	const target = "https://github.com/acme/widgets/pull/42"
	if err := open(target, "plan9", func(string, ...string) error { return nil }); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unsupported platform error = %v", err)
	}

	launcherErr := errors.New("launcher failed")
	err := open(target, "linux", func(string, ...string) error { return launcherErr })
	if err == nil || !errors.Is(err, launcherErr) {
		t.Fatalf("launcher error = %v, want wrapped launcher error", err)
	}
}
