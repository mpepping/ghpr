// Package auth resolves credentials used to access GitHub.
package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultHost is the GitHub host used when none is configured.
const DefaultHost = "github.com"

// Token returns the token for the active github.com account in the GitHub CLI
// hosts file. Environment variables are consulted only when that file does not
// exist.
func Token() (string, error) {
	return TokenForHost(DefaultHost)
}

// TokenForHost returns the token for the active account on host. GitHub
// Enterprise Server instances are stored under their own key in hosts.yml, so
// the host must be selected explicitly.
func TokenForHost(host string) (string, error) {
	host = normalizeHost(host)
	path, err := hostsPath(os.Getenv, os.UserHomeDir)
	if err != nil {
		return "", err
	}
	return tokenFromPath(path, host, os.Getenv)
}

// normalizeHost accepts values such as "https://github.example.com/" and
// reduces them to the bare hostname used as the hosts.yml key.
func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return DefaultHost
	}
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.Trim(host, "/")
	if host == "" {
		return DefaultHost
	}
	return strings.ToLower(host)
}

func hostsPath(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	if configDir := strings.TrimSpace(getenv("GH_CONFIG_DIR")); configDir != "" {
		return filepath.Join(configDir, "hosts.yml"), nil
	}
	if configHome := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); configHome != "" {
		return filepath.Join(configHome, "gh", "hosts.yml"), nil
	}

	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory for GitHub authentication: %w", err)
	}
	return filepath.Join(home, ".config", "gh", "hosts.yml"), nil
}

func tokenFromPath(path, host string, getenv func(string) string) (string, error) {
	contents, err := os.ReadFile(path)
	if err == nil {
		token, parseErr := tokenFromHosts(contents, host)
		if parseErr != nil {
			return "", fmt.Errorf("read GitHub token from %s: %w", path, parseErr)
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read GitHub authentication file %s: %w", path, err)
	}

	if token := strings.TrimSpace(getenv("GH_TOKEN")); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(getenv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	return "", fmt.Errorf("GitHub token not found; configure %s or set GH_TOKEN or GITHUB_TOKEN", path)
}

func tokenFromHosts(contents []byte, wanted string) (string, error) {
	var hosts map[string]hostConfig
	if err := yaml.Unmarshal(contents, &hosts); err != nil {
		return "", fmt.Errorf("parse hosts.yml: %w", err)
	}

	host, ok := hosts[wanted]
	if !ok {
		return "", fmt.Errorf("hosts.yml has no %s entry", wanted)
	}

	if host.User != "" {
		if user, exists := host.Users[host.User]; exists {
			if token := strings.TrimSpace(user.OAuthToken); token != "" {
				return token, nil
			}
		}
	}
	if token := strings.TrimSpace(host.OAuthToken); token != "" {
		return token, nil
	}
	if len(host.Users) == 1 {
		for _, user := range host.Users {
			if token := strings.TrimSpace(user.OAuthToken); token != "" {
				return token, nil
			}
		}
	}

	if host.User != "" {
		return "", fmt.Errorf("no OAuth token found for active %s user %q", wanted, host.User)
	}
	return "", fmt.Errorf("no OAuth token found for %s", wanted)
}

type hostConfig struct {
	User       string                `yaml:"user"`
	OAuthToken string                `yaml:"oauth_token"`
	Users      map[string]userConfig `yaml:"users"`
}

type userConfig struct {
	OAuthToken string `yaml:"oauth_token"`
}
