package main

import (
	"os"
	"path/filepath"
)

// File discovery follows the selected project's ignore sources even when the
// execution work root covers the whole VPS. No second project registry is kept.
func projectIgnoreRoot(workRoot, selected string) string {
	workRoot, selected = filepath.Clean(workRoot), filepath.Clean(selected)
	for directory := selected; ; directory = filepath.Dir(directory) {
		if info, err := os.Stat(filepath.Join(directory, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return directory
		}
		if directory == workRoot || filepath.Dir(directory) == directory {
			break
		}
	}
	return workRoot
}

type IgnoreConfig struct {
	UseGitignore       *bool    `json:"use_gitignore,omitempty"`
	UseDefaultPatterns *bool    `json:"use_default_patterns,omitempty"`
	CustomPatterns     []string `json:"custom_patterns,omitempty"`
}

func (c Config) ignoreOptions() IgnoreOptions {
	if c.Ignore == nil {
		return IgnoreOptions{}
	}
	return IgnoreOptions{
		DisableGitignore:       c.Ignore.UseGitignore != nil && !*c.Ignore.UseGitignore,
		DisableDefaultPatterns: c.Ignore.UseDefaultPatterns != nil && !*c.Ignore.UseDefaultPatterns,
		CustomPatterns:         c.Ignore.CustomPatterns,
	}
}
