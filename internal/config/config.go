// Package config loads ghpr's optional configuration file.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the on-disk configuration. Every field is optional; command line
// flags always win over the values found here.
type File struct {
	Owner        string `yaml:"owner"`
	Limit        int    `yaml:"limit"`
	Scope        string `yaml:"scope"`
	MergeMethod  string `yaml:"merge_method"`
	DeleteBranch bool   `yaml:"delete_branch"`
	Host         string `yaml:"host"`
	Filter       string `yaml:"filter"`
	NoColor      bool   `yaml:"no_color"`
	Editor       string `yaml:"editor"`
}

// Path resolves the configuration file location, mirroring how the GitHub CLI
// finds its own configuration.
//
//  1. $GHPR_CONFIG
//  2. $XDG_CONFIG_HOME/ghpr/config.yml
//  3. $HOME/.config/ghpr/config.yml
func Path(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	if explicit := strings.TrimSpace(getenv("GHPR_CONFIG")); explicit != "" {
		return explicit, nil
	}
	if configHome := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); configHome != "" {
		return filepath.Join(configHome, "ghpr", "config.yml"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory for the ghpr configuration: %w", err)
	}
	return filepath.Join(home, ".config", "ghpr", "config.yml"), nil
}

// Load reads the configuration file. A missing file is not an error: ghpr is
// fully usable without one.
func Load() (File, string, error) {
	path, err := Path(os.Getenv, os.UserHomeDir)
	if err != nil {
		return File{}, "", err
	}
	file, err := loadPath(path)
	return file, path, err
}

func loadPath(path string) (File, error) {
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil
		}
		return File{}, fmt.Errorf("read ghpr configuration %s: %w", path, err)
	}
	defer handle.Close()

	return decode(handle, path)
}

func decode(reader io.Reader, path string) (File, error) {
	var file File
	decoder := yaml.NewDecoder(reader)
	// Reject unknown keys so a typo is reported instead of silently ignored.
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		if errors.Is(err, io.EOF) {
			return File{}, nil
		}
		return File{}, fmt.Errorf("parse ghpr configuration %s: %w", path, err)
	}
	if file.Limit < 0 {
		return File{}, fmt.Errorf("parse ghpr configuration %s: limit must not be negative", path)
	}
	return file, nil
}
