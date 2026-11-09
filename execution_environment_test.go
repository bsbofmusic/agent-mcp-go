package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionFindsUserToolsWithServicePATH(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"go": "user-go", "skills-manager-cli": "user-skills", "git": "user-git"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '"+text+"\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	// A real service PATH may contain git. Use an empty directory here so
	// this regression verifies user-directory fallback rather than shadowing
	// an existing executable, whose precedence must remain unchanged.
	t.Setenv("PATH", t.TempDir())
	app, _, _ := testApp(t)
	session, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: "go version; skills-manager-cli; git status", YieldTimeMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer app.sessions.shutdown()
	result := session.wait(1000)
	if !result.Finished || result.ExitCode != 0 || !strings.Contains(result.Output, "user-go") || !strings.Contains(result.Output, "user-skills") {
		t.Fatalf("cold shell cannot discover installed user tools: %+v", result)
	}
	gitResult, err := app.parityGit(t.Context(), "", "status")
	if err != nil || !strings.Contains(gitResult.Output, "user-git") {
		t.Fatalf("native Git lookup disagrees with Shell: %+v %v", gitResult, err)
	}
	if text, err := app.gitOp(t.Context(), "", "status"); err != nil || !strings.Contains(text, "user-git") {
		t.Fatalf("git_status lookup disagrees: %q %v", text, err)
	}
	out, failed, err := app.parityRunCommand(t.Context(), RunCommandInput{Command: "go", Args: []string{"version"}})
	if err != nil || failed || !strings.Contains(out.Output, "user-go") {
		t.Fatalf("run_command disagrees with shell PATH: %+v failed=%v err=%v", out, failed, err)
	}
}
