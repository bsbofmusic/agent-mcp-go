# AMG · Agent MCP Go

**English** | [简体中文](README.zh-CN.md)

**Give any AI agent a full-powered Linux workspace over standard MCP.** One static Go binary turns your host into an authenticated remote execution environment with files, Shell, Git, chunked transfers, and dynamic Skill/MCP orchestration.

**Upstream-Standard · Unified Orchestration · Full-Powered Execution**

![AMG architecture: compatible MCP agents connect to the standalone core, with open extension contracts for Skills, external MCP routes, and workspace memory](assets/amg-open-architecture.svg)

[Quick start](#quick-start) · [Core pillars](#why-amg) · [40 native tools](#full-powered-workspace-capabilities) · [Integrations](#optional-integrations) · [Configuration](docs/configuration.md) · [Security](docs/security.md) · [v1.2.3 Release Notes](docs/releases/v1.2.3.md)

## Why AMG

Most remote agent setups either wrap fragile shell scripts in custom protocols or force you to deploy a heavy multi-service stack just to edit a file. AMG takes the opposite approach:

1. **Upstream-Standard, Zero Reinvented Wheels**
   - Built directly on the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`), standard Streamable HTTP (`/mcp` stateful and `/mcp/stateless`), and standard OAuth 2.1 + PKCE (`S256`) + RFC 7591 Dynamic Client Registration + RFC 9728/8414 discovery.
   - Compiles into a single `CGO_ENABLED=0` static binary with 5 direct Go dependencies (`go-sdk`, `jsonschema-go`, `doublestar/v4`, `yaml.v3`, `x/time/rate`), executing real POSIX shells (`os/exec`) and native `git` commands.
   - Exposes open file and CLI contracts (`skills_root`, `shared_mcp_bin`, on-disk archive) instead of proprietary glue. Run it standalone or plug in your own tools with zero daemon sprawl.

2. **Unified Orchestration Across Agents and Ecosystems**
   - **One workspace, any client:** Connect ChatGPT, Gemini Web, Claude, Doubao Work, Qoder, or any MCP-compatible web, desktop, or CLI agent to the same Linux environment without vendor lock-in.
   - **Single source of truth, zero duplicate registries:** AMG never mirrors external catalogs or runs stale background indexers. At `initialize`, it sends a bounded, compact Skills index (`skill_index_entry_bytes` / `skill_index_max_bytes`) alongside workspace `AGENTS.md` and optional `bootstrap_files`, then loads full Skill bodies (`skills_read`) and live external MCP schemas (`mcp_list_sources`, `mcp_search_tools`, `mcp_get_tool`, `mcp_call_tool`) strictly on demand. Changes to Skills or upstream MCP routes take effect on the very next call with zero restarts.
   - **Multi-agent concurrency with scope isolation:** Multiple agents can hit one server simultaneously. Per-scope locking isolates task plans, notes, and handover checkpoints (`scope`); per-repository locking and per-scope Git refs isolate change reviews; `git worktree` support keeps parallel code edits collision-free.

3. **Full-Powered Execution, Hardened for Production**
   - **40 native workspace tools:** Complete filesystem navigation, regex search, line-bounded reads, atomic Codex-style `apply_patch` with rollback, image inspection, Git workflow (`status`, `log`, `commit`, `push`), resumable interactive Shell sessions (`exec_command` + `write_stdin` with 46-bit float-safe handles), chunked transfers up to 300 MiB+ (`transfer_prepare` with 16 MiB PATCH chunks, SHA-256 verification, and no-clobber defaults), and MCP Review UI (`show_changes`).
   - **Zero-friction hosted client sign-in:** Built-in OAuth 2.1 server with 30-day access tokens, non-rotating 90-day refresh tokens, CORS preflight support, and configurable `annotations_mode` (`default` semantic hints vs. `trusted` low-friction mode for private single-owner hosts) so browser clients like Gemini Web connect with empty Client ID/Secret and run smoothly.
   - **Fast and bounded under heavy load:** `os.OpenRoot` confinement blocks symlink escapes; `(inode, size, mtime, ctime)` stat fast-paths serve cached Skills metadata with zero file opens; bounded review baselines (`review_baseline_timeout_seconds` and `review_baseline_skip_paths`) degrade cleanly to `HEAD^{tree}` on multi-gigabyte repositories instead of stalling; 18 runtime settings hot-update via `settings_update` without a restart.

## Full-powered workspace capabilities

AMG exposes 40 tools out of the box across six operational surfaces:

| Domain | Tools | Highlights |
| --- | --- | --- |
| **Filesystem & Patching** | `list_directory`, `tree`, `glob`, `grep`, `read_file`, `write_file`, `apply_patch`, `view_image`, `get_project_doc` | `os.OpenRoot` confinement, gitignore-aware search, atomic multi-file patching with rollback, bounded image blocks, hierarchical `AGENTS.md` loading |
| **Shell & Process Control** | `exec_command`, `write_stdin`, `stop_command`, `run_command`, `clock_curr_time`, `clock_sleep` | POSIX shell execution, configurable yield windows, resumable background sessions, Ctrl-C / process-group termination, direct argv binary execution |
| **Git & Change Review** | `git_status`, `git_log`, `git_commit`, `git_push`, `show_changes` | Per-repo baseline serialization, per-scope `refs/amg/review/*` tracking, bounded snapshot deadlines, MCP Review UI resource |
| **Transfers & Attachments** | `transfer_prepare`, `export_host_file`, `import_host_file`, `import_chatgpt_file` | Capability-URL HTTP upload/download up to `max_file_bytes` (default 300 MiB), 16 MiB PATCH chunks, SHA-256 verification, no-clobber default (`409` unless `overwrite=true`) |
| **Skills & Shared MCP** | `skills_list`, `skills_read`, `mcp_list_sources`, `mcp_search_tools`, `mcp_get_tool`, `mcp_call_tool` (plus `shared_mcp_*` aliases) | Stat-cached Skills discovery, on-demand support file reads, live upstream MCP module discovery and native result passthrough |
| **Task Continuity & Tuning** | `get_agent_brief`, `update_plan`, `remember`, `recall`, `get_environment`, `settings_get`, `settings_update` | Per-scope serialized plans and notes, 30-day automatic cleanup for anonymous sessions, 18 live hot-tunable settings without restart |

## Optional integrations

AMG runs completely standalone. When you want shared Skills, external MCP servers, or cross-session memory, connect any provider that satisfies these open contracts:

| Capability | Open integration contract | Recommended example |
| --- | --- | --- |
| **Skills** | [`skills_root`](docs/integrations.md#local-skills): local directory of `SKILL.md` packages | Skills Manager |
| **External MCP tools** | [`shared_mcp_bin`](docs/integrations.md#shared-mcp-cli-contract): stdio route adapter executable | [AgentGateway](https://github.com/agentgateway/agentgateway) + compatible adapter |
| **Persistent memory** | [On-disk archive](docs/integrations.md#task-continuity-and-memory): Markdown and transcript archive inside `work_root` | [Obsidian](https://obsidian.md/) + [AgentsView](https://github.com/kenn-io/agentsview) |

AMG does not install, bundle, or manage these providers. Leaving `skills_root` empty or `shared_mcp_bin` unset keeps the core workspace server completely self-contained.

## Quick start

**Requirements:** Linux amd64, `curl`, GNU `sha256sum`, `tar`, and Python 3 (plus POSIX shell and `git` on the host). Run AMG under a dedicated non-root OS account; a valid token grants command execution as that user (see [Security](docs/security.md)).

### 1. Download and verify

```sh
mkdir amg-v1.2.3
cd amg-v1.2.3
release=https://github.com/bsbofmusic/agent-mcp-go/releases/download/v1.2.3
curl -fLO "$release/agent-mcp-go-v1.2.3-linux-amd64.tar.gz"
curl -fLO "$release/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf agent-mcp-go-v1.2.3-linux-amd64.tar.gz
install -Dm755 agent-mcp-go "$HOME/.local/bin/agent-mcp-go"
```

### 2. Create a personal configuration

The packaged [config.example.json](config.example.json) uses `/home/amg` placeholders. This script resolves your actual home directory, creates `0700` workspace/state/Skills directories, and generates a `0600` owner token without overwriting existing files:

```sh
python3 - <<'PY'
import json, os, secrets
from pathlib import Path

os.umask(0o077)
home = Path.home().resolve()
config_dir = home / ".config/agent-mcp-go"
config_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
config_path = config_dir / "config.json"
token_path = config_dir / "mcp-token"
if config_path.exists() or token_path.exists():
    raise SystemExit("Existing config/token found; review them instead of overwriting.")
config = json.loads(Path("config.example.json").read_text())
config.update({
    "listen": "127.0.0.1:3100",
    "work_root": str(home / "workspace"),
    "default_workdir": str(home / "workspace"),
    "state_dir": str(home / ".local/state/agent-mcp-go"),
    "skills_root": str(home / ".config/agent-mcp-go/skills"),
    "token_file": str(token_path),
    "public_url": "http://127.0.0.1:3100",
    "allowed_origins": [],
    "shared_mcp_bin": "",
    "executable_dirs": [],
})
for key in ("work_root", "state_dir", "skills_root"):
    Path(config[key]).mkdir(parents=True, exist_ok=True, mode=0o700)
with token_path.open("x") as f:
    f.write(secrets.token_hex(32) + "\n")
token_path.chmod(0o600)
with config_path.open("x") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
config_path.chmod(0o600)
print("Configuration:", config_path)
PY
```

AMG expects literal absolute paths in JSON and does not expand `~` or `$HOME`.

### 3. Start and check health

```sh
"$HOME/.local/bin/agent-mcp-go" --config "$HOME/.config/agent-mcp-go/config.json"
```

In another terminal:

```sh
curl -fsS http://127.0.0.1:3100/healthz
```

For persistent background operation with cgroup memory guardrails (`MemoryHigh=400M`, `MemoryMax=800M`, `MemorySwapMax=512M`), see [systemd/agent-mcp-go.service.example](systemd/agent-mcp-go.service.example).

### 4. Connect a client

**Static Bearer header (CLI, desktop, or local clients):**

| Setting | Value |
| --- | --- |
| **URL** | `http://127.0.0.1:3100/mcp` (or `/mcp/stateless` if the client does not retain `Mcp-Session-Id`) |
| **Header** | `Authorization: Bearer <contents of mcp-token>` |

**Hosted web clients via OAuth 2.1 (Google Gemini Web, Claude Web, ChatGPT):**

1. Expose `127.0.0.1:3100` behind your HTTPS reverse proxy or tunnel, set `public_url` to that HTTPS origin, add the client origin to `allowed_origins`, and enable `"oauth": { "enabled": true }` in `config.json`.
2. In the client's custom MCP connector (for example, Gemini Web Connected Apps), enter `<public_url>/mcp/stateless` (or `<public_url>/mcp`) and leave Client ID and Client Secret blank to use zero-credential Dynamic Client Registration.
3. Approve the consent prompt using your `mcp-token` file contents. Access tokens last 30 days and refresh tokens last 90 days by default.

See [Gemini Web and OAuth clients](docs/integrations.md#gemini-web-and-oauth-clients) for full setup details and [Gemini repeated tool confirmations](docs/integrations.md#gemini-repeated-tool-confirmations) for host-side approval settings.

## Concurrent multi-agent operation

- **Per-scope state isolation:** Pass an explicit `scope` string so each agent gets serialized `update_plan`, `remember`, `recall`, and `show_changes` state without colliding with parallel sessions. Anonymous sessions retain scratch state for 30 days before automatic cleanup.
- **Parallel repository edits:** Give each concurrent coding agent its own `git worktree` checkout and pass that path via `workdir` (`exec_command` / `run_command`) or `directory` (Git tools).
- **Single-writer discipline:** Keep shared background services and `settings_update` calls under a single coordinator.

## Build from source and test

Requires **Go 1.25+**:

```sh
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o agent-mcp-go .
```

Opt-in live integration tests require `AGENT_MCP_GO_CONFIG` pointing to a disposable test workspace (see [v1.2.3 release notes](docs/releases/v1.2.3.md#installation-and-verification)).

## Documentation

- [Configuration reference](docs/configuration.md): all fields, OAuth 2.1 options, resource limits, and systemd unit setup.
- [Client & ecosystem integrations](docs/integrations.md): Gemini Web, Claude Web, ChatGPT, local Skills, shared MCP CLI contract, and scope continuity.
- [Security model](docs/security.md): OS confinement, token handling, capability transfer URLs, and concurrency bounds.
- [Changelog](CHANGELOG.md) and [v1.2.3 release notes](docs/releases/v1.2.3.md).

## License and acknowledgments

Released under the [MIT License](LICENSE). Workspace tool semantics and documentation structure draw inspiration from [Codex Free](https://github.com/hypnguyen1209/codex-free). The bundled review UI is adapted from Codex Free v1.9.1 (MIT, copyright © 2026 hypnguyen1209) and includes [Prism](https://prismjs.com/) (MIT, copyright © 2012 Lea Verou); notices are preserved in [assets/CODEXFREE-REVIEW-LICENSE.txt](assets/CODEXFREE-REVIEW-LICENSE.txt).
