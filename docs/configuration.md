# Configuration

AMG reads one JSON configuration selected by `--config /absolute/path/config.json` or `AGENT_MCP_GO_CONFIG`. An explicit `--config` takes precedence. There is no automatic home-directory configuration lookup.

Start from [config.example.json](../config.example.json) and follow the [README setup](../README.md#quick-start). The example's `/home/amg` paths are placeholders. Use absolute paths for all configured filesystem locations; JSON strings do not expand `~`, `$HOME`, or other environment variables.

## Fields

| Key | Meaning and constraints |
| --- | --- |
| `listen` | Required. Loopback address beginning with `127.0.0.1:` or `[::1]:`, for example `127.0.0.1:3100`. Public listeners are refused. |
| `work_root` | Required absolute path. Root for workspace file tools; create it before use. It can contain several projects. |
| `state_dir` | Required absolute path. Private task state and temporary export snapshots. Created on startup when absent. Keep outside the workspace. |
| `token_file` | Required path to the static Bearer token. Use an absolute path outside the workspace, mode `0600`, containing at least 32 characters after trimming whitespace. Startup rejects group/other permissions and short tokens. |
| `public_url` | External origin used for generated transfer URLs and HTTP Host/Origin checks, without `/mcp`; for local use, `http://127.0.0.1:3100`. For remote use, set your own HTTPS origin. |
| `skills_root` | Optional existing directory containing immediate child Skill directories, each with `SKILL.md`. An empty directory is sufficient for a standalone installation. |
| `shared_mcp_bin` | Optional absolute path to an external MCP stdio CLI implementing the interface described in [integrations](integrations.md#shared-mcp-cli-contract). Leave empty when unavailable. |
| `executable_dirs` | Optional list of absolute executable directories. Entries must not contain the OS path-list separator (`:` on Linux). |
| `max_file_bytes` | Required integer, **1,048,576–1,073,741,824** (1 MiB–1 GiB). Sets the configured transfer/file limit. |
| `max_output_bytes` | Required integer, **1,024–4,194,304** (1 KiB–4 MiB). Bounds tool output. |
| `command_timeout_seconds` | Required integer, **1–3,600**. Command runtime limit. |
| `ignore` | Optional search/listing ignore configuration; see below. |

Required tuning values must be present and positive within these ranges; zero is invalid. A 300 MiB `max_file_bytes` value is `314572800`. External proxy and client limits can be lower than AMG's configured limit.

The current parser enforces absolute `work_root` and `state_dir`; use an absolute `token_file` as a deployment requirement too. Keep configuration and token files owner-only (`0600`) and private parent directories `0700`.

## Executable lookup

Commands inherit the server process environment. Executable lookup keeps absolute entries from its `PATH`, then adds `executable_dirs` and standard user locations such as the user's `.local/bin` and `.nix-profile/bin`, removing duplicates. It never implicitly adds the current directory. Check `command -v <program>` through the remote Shell when a service environment differs from a login shell.

AMG does not install development tools. Install Git, language runtimes, and other commands under the OS account's normal administration process.

## Ignore options

The `ignore` object accepts `use_gitignore`, `use_default_patterns`, and `custom_patterns`. The first two are optional booleans; enabled behavior is the default when omitted. Custom patterns are a list of glob-style strings. The parser caps custom patterns at 4,096 entries and 1 MiB in aggregate. Search traversal always prunes `.git` and `node_modules` directories.

Ignore rules control discovery and output; they are not access-control rules. Explicit tools and Shell remain governed by their own path and OS permissions.

## Settings and restart behavior

The settings tool can update the three tuning values and persist them to the selected configuration file. Structural fields such as listener, roots, and ingress origin should be changed in the file and applied by restarting AMG. The Bearer token is read at startup; restart after rotation and update authorized clients.

## Optional user service

The supplied [systemd user unit](../systemd/agent-mcp-go.service.example) expects the binary at `%h/.local/bin/agent-mcp-go` and configuration at `%h/.config/agent-mcp-go/config.json`. `%h` is expanded by systemd; it is not a JSON path expansion mechanism.

After the foreground setup succeeds, install and start the unit:

```sh
install -Dm644 systemd/agent-mcp-go.service.example "$HOME/.config/systemd/user/agent-mcp-go.service"
systemctl --user daemon-reload
systemctl --user enable --now agent-mcp-go.service
systemctl --user status agent-mcp-go.service
journalctl --user -u agent-mcp-go.service -n 50
```

Stop the foreground instance before starting the service on the same port. Persistent operation after logout may require an administrator to enable user lingering. TLS termination and tunnels are supplied and managed separately by the operator.

See the [security model](security.md) before making the service reachable remotely.
