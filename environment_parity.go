package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Match the active Rust CodexFree's Bash execution when the platform has it;
// retain /bin/sh as the portable fallback.
func defaultExecShell() string {
	if bash, err := exec.LookPath("bash"); err == nil {
		return bash
	}
	return "/bin/sh"
}

var allowedRunBinaries = []string{"git", "go", "make", "node", "npm", "npx", "python", "python3", "pip", "cargo", "bun"}

func runBinaryAllowed(name string) bool {
	for _, n := range allowedRunBinaries {
		if name == n {
			return true
		}
	}
	return false
}

type ExecPolicy struct {
	Mode            string   `json:"mode"`
	AllowedCommands []string `json:"allowed_commands"`
	MaxSessions     int      `json:"max_sessions"`
}
type ShellPolicy struct {
	Bin        string   `json:"bin"`
	Type       string   `json:"type"`
	ArgvPrefix []string `json:"argv_prefix"`
}
type EnvironmentOutput struct {
	OS                string      `json:"os"`
	Arch              string      `json:"arch"`
	Cwd               string      `json:"cwd"`
	Exec              ExecPolicy  `json:"exec"`
	PathSeparator     string      `json:"path_separator"`
	Platform          string      `json:"platform"`
	RunCommandAllowed []string    `json:"run_command_allowed"`
	Shell             ShellPolicy `json:"shell"`
	ExecutionPath     string      `json:"execution_path"`
}

func (a *App) registerEnvironmentParity(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_environment",
		Description: "Report host OS, current work root and default working directory, Shell policy, unrestricted execution mode, and bounded command allowlist without credentials. If the host did not surface the MCP initialize instructions, call get_agent_brief first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, EnvironmentOutput, error) {
		arch := runtime.GOARCH
		if arch == "amd64" {
			arch = "x64"
		}
		osname := "Linux"
		if runtime.GOOS == "darwin" {
			osname = "macOS"
		} else if runtime.GOOS == "windows" {
			osname = "Windows"
		}
		return nil, EnvironmentOutput{
			OS: osname, Arch: arch, Cwd: a.config.Get().defaultWorkdir(),
			Exec:          ExecPolicy{Mode: "unrestricted", AllowedCommands: []string{}, MaxSessions: 8},
			PathSeparator: string(os.PathSeparator), Platform: runtime.GOOS,
			RunCommandAllowed: append([]string{}, allowedRunBinaries...),
			Shell:             ShellPolicy{Bin: defaultExecShell(), Type: "posix", ArgvPrefix: []string{"-c"}},
			ExecutionPath:     executionPath(a.config.Get()),
		}, nil
	})
}
