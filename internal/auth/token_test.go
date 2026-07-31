package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenFromHostsUsesActiveUser(t *testing.T) {
	t.Parallel()

	contents := []byte(`github.com:
  user: octocat
  git_protocol: ssh
  users:
    other:
      oauth_token: token-other
    octocat:
      oauth_token: token-active
`)
	token, err := tokenFromHosts(contents)
	if err != nil {
		t.Fatalf("tokenFromHosts() error = %v", err)
	}
	if token != "token-active" {
		t.Fatalf("tokenFromHosts() = %q, want active user's token", token)
	}
}

func TestTokenFromHostsSupportsLegacyFormat(t *testing.T) {
	t.Parallel()

	contents := []byte(`github.com:
  user: octocat
  oauth_token: token-legacy
  git_protocol: https
`)
	token, err := tokenFromHosts(contents)
	if err != nil {
		t.Fatalf("tokenFromHosts() error = %v", err)
	}
	if token != "token-legacy" {
		t.Fatalf("tokenFromHosts() = %q, want legacy token", token)
	}
}

func TestTokenFromPathPrefersExistingHostsFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hosts.yml")
	contents := []byte("github.com:\n  oauth_token: token-from-file\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write hosts file: %v", err)
	}

	token, err := tokenFromPath(path, envValues(map[string]string{"GH_TOKEN": "token-from-env"}))
	if err != nil {
		t.Fatalf("tokenFromPath() error = %v", err)
	}
	if token != "token-from-file" {
		t.Fatalf("tokenFromPath() = %q, want file token", token)
	}
}

func TestTokenFromPathUsesEnvironmentWhenFileDoesNotExist(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "hosts.yml")
	token, err := tokenFromPath(path, envValues(map[string]string{
		"GH_TOKEN":     " token-from-gh ",
		"GITHUB_TOKEN": "token-from-github",
	}))
	if err != nil {
		t.Fatalf("tokenFromPath() error = %v", err)
	}
	if token != "token-from-gh" {
		t.Fatalf("tokenFromPath() = %q, want GH_TOKEN", token)
	}
}

func TestTokenFromPathDoesNotUseEnvironmentForInvalidExistingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hosts.yml")
	if err := os.WriteFile(path, []byte("github.com:\n  user: octocat\n"), 0o600); err != nil {
		t.Fatalf("write hosts file: %v", err)
	}

	_, err := tokenFromPath(path, envValues(map[string]string{"GH_TOKEN": "token-from-env"}))
	if err == nil || !strings.Contains(err.Error(), "no OAuth token") {
		t.Fatalf("tokenFromPath() error = %v, want missing OAuth token error", err)
	}
}

func TestHostsPathPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		home    string
		want    string
		homeErr error
	}{
		{
			name: "GitHub config directory",
			env:  map[string]string{"GH_CONFIG_DIR": "/custom/gh", "XDG_CONFIG_HOME": "/xdg"},
			want: filepath.Join("/custom/gh", "hosts.yml"),
		},
		{
			name: "XDG config directory",
			env:  map[string]string{"XDG_CONFIG_HOME": "/xdg"},
			want: filepath.Join("/xdg", "gh", "hosts.yml"),
		},
		{
			name: "home config directory",
			home: "/home/octocat",
			want: filepath.Join("/home/octocat", ".config", "gh", "hosts.yml"),
		},
		{
			name:    "home lookup failure",
			homeErr: errors.New("no home"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path, err := hostsPath(envValues(test.env), func() (string, error) {
				return test.home, test.homeErr
			})
			if test.homeErr != nil {
				if err == nil {
					t.Fatal("hostsPath() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("hostsPath() error = %v", err)
			}
			if path != test.want {
				t.Fatalf("hostsPath() = %q, want %q", path, test.want)
			}
		})
	}
}

func envValues(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
