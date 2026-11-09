package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// executionPath extends a service manager's minimal PATH with configured and
// standard user executable directories, preserving the operator's precedence.
// It is shared by process creation and the model-facing environment report.
func executionPath(c Config) string {
	parts := filepath.SplitList(os.Getenv("PATH"))
	parts = append(parts, c.ExecutableDirs...)
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		parts = append(parts, filepath.Join(home, ".local", "bin"), filepath.Join(home, ".nix-profile", "bin"))
	}
	var unique []string
	seen := map[string]bool{}
	for _, part := range parts {
		// Never implicitly add the current working directory to command lookup.
		if !filepath.IsAbs(part) {
			continue
		}
		part = filepath.Clean(part)
		if !seen[part] {
			unique = append(unique, part)
			seen[part] = true
		}
	}
	return strings.Join(unique, string(os.PathListSeparator))
}

func executionEnvironment(c Config) []string {
	return append(os.Environ(), "PATH="+executionPath(c))
}

func executionBinary(c Config, name string) (string, error) {
	if strings.ContainsAny(name, `/\\`) {
		return exec.LookPath(name)
	}
	for _, dir := range filepath.SplitList(executionPath(c)) {
		if path, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}
