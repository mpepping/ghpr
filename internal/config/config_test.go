package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	contents := `
owner: mpepping
limit: 250
scope: review-requested
merge_method: rebase
delete_branch: true
host: github.example.com
filter: dependabot
no_color: true
editor: nvim
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := loadPath(path)
	if err != nil {
		t.Fatalf("loadPath() error = %v", err)
	}
	want := File{
		Owner:        "mpepping",
		Limit:        250,
		Scope:        "review-requested",
		MergeMethod:  "rebase",
		DeleteBranch: true,
		Host:         "github.example.com",
		Filter:       "dependabot",
		NoColor:      true,
		Editor:       "nvim",
	}
	if file != want {
		t.Fatalf("loadPath() = %#v, want %#v", file, want)
	}
}

// ghpr must work without a configuration file.
func TestMissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	file, err := loadPath(filepath.Join(t.TempDir(), "absent.yml"))
	if err != nil {
		t.Fatalf("a missing config must be ignored, got %v", err)
	}
	if file != (File{}) {
		t.Fatalf("expected a zero config, got %#v", file)
	}
}

func TestEmptyFileIsNotAnError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPath(path); err != nil {
		t.Fatalf("an empty config must be ignored, got %v", err)
	}
}

// A typo should be reported instead of silently doing nothing.
func TestUnknownKeyIsRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("ownr: mpepping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadPath(path)
	if err == nil || !strings.Contains(err.Error(), "ownr") {
		t.Fatalf("error = %v, want the unknown key to be named", err)
	}
}

func TestInvalidYAMLIsReported(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("owner: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPath(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestNegativeLimitIsRejected(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("limit: -5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPath(path); err == nil {
		t.Fatal("expected a validation error for a negative limit")
	}
}

func TestPathResolutionOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  map[string]string
		home string
		want string
	}{
		{
			"explicit override wins",
			map[string]string{"GHPR_CONFIG": "/tmp/custom.yml", "XDG_CONFIG_HOME": "/xdg"},
			"/home/user",
			"/tmp/custom.yml",
		},
		{
			"xdg config home",
			map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			"/home/user",
			filepath.Join("/xdg", "ghpr", "config.yml"),
		},
		{
			"home directory fallback",
			map[string]string{},
			"/home/user",
			filepath.Join("/home/user", ".config", "ghpr", "config.yml"),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(key string) string { return testCase.env[key] }
			home := func() (string, error) { return testCase.home, nil }

			got, err := Path(getenv, home)
			if err != nil {
				t.Fatalf("Path() error = %v", err)
			}
			if got != testCase.want {
				t.Fatalf("Path() = %q, want %q", got, testCase.want)
			}
		})
	}
}
