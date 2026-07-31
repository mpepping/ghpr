// Package browser opens URLs using the operating system's default browser.
package browser

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// Open opens rawURL in the default browser.
func Open(rawURL string) error {
	return open(rawURL, runtime.GOOS, func(name string, args ...string) error {
		command := exec.Command(name, args...)
		if err := command.Start(); err != nil {
			return err
		}
		return command.Process.Release()
	})
}

func open(rawURL, goos string, run func(string, ...string) error) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("invalid HTTP URL %q", rawURL)
	}

	var name string
	var args []string
	switch goos {
	case "darwin":
		name = "open"
		args = []string{rawURL}
	case "linux":
		name = "xdg-open"
		args = []string{rawURL}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return fmt.Errorf("opening a browser is not supported on %s", goos)
	}

	if err := run(name, args...); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("browser launcher %q was not found: %w", name, err)
		}
		return fmt.Errorf("run browser launcher %q: %w", name, err)
	}
	return nil
}
