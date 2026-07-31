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

const githubHost = "github.com"

// Token returns the token for the active github.com account in the GitHub CLI
// hosts file. Environment variables are consulted only when that file does not
// exist.
func Token() (string, error) {
	path, err := hostsPath(os.Getenv, os.UserHomeDir)
	if err != nil {
		return "", err
	}
	return tokenFromPath(path, os.Getenv)
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

func tokenFromPath(path string, getenv func(string) string) (string, error) {
	contents, err := os.ReadFile(path)
	if err == nil {
		token, parseErr := tokenFromHosts(contents)
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

func tokenFromHosts(contents []byte) (string, error) {
	var hosts map[string]hostConfig
	if err := yaml.Unmarshal(contents, &hosts); err != nil {
		return "", fmt.Errorf("parse hosts.yml: %w", err)
	}

	host, ok := hosts[githubHost]
	if !ok {
		return "", errors.New("hosts.yml has no github.com entry")
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
		return "", fmt.Errorf("no OAuth token found for active github.com user %q", host.User)
	}
	return "", errors.New("no OAuth token found for github.com")
}

type hostConfig struct {
	User       string                `yaml:"user"`
	OAuthToken string                `yaml:"oauth_token"`
	Users      map[string]userConfig `yaml:"users"`
}

type userConfig struct {
	OAuthToken string `yaml:"oauth_token"`
}
