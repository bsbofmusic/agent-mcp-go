package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Live tests never discover credentials from the developer's home directory.
func liveTestConfig(t *testing.T) (Config, error) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("AGENT_MCP_GO_CONFIG"))
	if path == "" {
		t.Skip("live tests require an explicit AGENT_MCP_GO_CONFIG path")
	}
	return readConfig(path)
}

func TestProjectDocFindsUserHomeWithinWorkspace(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	home := filepath.Join(root, "portable-home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("PORTABLE_HOME_RULES"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	doc, err := app.projectDocAt("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Content, "PORTABLE_HOME_RULES") {
		t.Fatal("home project instructions inside the workspace were not discovered")
	}
}
