# AMG · Agent MCP Go

A general-purpose remote agent workspace server over [MCP](https://modelcontextprotocol.io/). Connect an MCP client to a Linux workspace to inspect files, edit projects, run commands, review changes, and move files without putting the workspace on the client machine.

AMG uses the official Go MCP SDK. The core server runs independently; existing Skills, shared MCP services, and MemOS can be connected when available.

[Quick start](#quick-start) · [Configuration](docs/configuration.md) · [Integrations](docs/integrations.md) · [Security](docs/security.md) · [Release notes](docs/releases/v1.1.0.md)

## What it provides

- Workspace browsing, search, text reads and writes, structured patches, and image inspection.
- Shell execution with bounded output, resumable process sessions, stdin, and cancellation; Git operations and project instructions.
- Streaming file transfer, chunked uploads, and MCP resource export.
- Change review, task plans, notes, and scoped handover across connections.
- Fresh agent instructions at initialization/discovery, capped at 24 KiB, including a Skills catalog preview capped at 13 KiB. Full Skill bodies are read on demand.
- Optional live discovery and calls to an existing shared MCP installation, without a bundled shared stack.

**Validated platform:** Linux amd64. Building requires **Go 1.25+**. Shell tools require a POSIX shell; Git operations require Git on the server. Interactive TTY programs are unsupported.

**Trust model:** a Bearer token grants code execution as the server's OS user. Run AMG under a dedicated account and read the [security model](docs/security.md) before exposing it remotely.

## Quick start

The commands below require Linux amd64, `curl`, GNU `sha256sum`, `tar`, and Python 3. Use a new installation directory.

### 1. Download and verify

```sh
mkdir amg-v1.1.0
cd amg-v1.1.0
release=https://github.com/bsbofmusic/agent-mcp-go/releases/download/v1.1.0
curl -fLO "$release/agent-mcp-go-v1.1.0-linux-amd64.tar.gz"
curl -fLO "$release/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf agent-mcp-go-v1.1.0-linux-amd64.tar.gz
install -Dm755 agent-mcp-go "$HOME/.local/bin/agent-mcp-go"
```

Continue only when the archive checksum reports `OK`. Checksums detect changed bytes; obtain both files from the intended release.

### 2. Create a personal configuration

The packaged [example configuration](config.example.json) uses placeholder paths under `/home/amg`. This script replaces them with absolute paths for your actual home, creates the workspace/state/Skills directories, and creates an owner-only random token. It refuses to overwrite an existing configuration or token.

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
    "state_dir": str(home / ".local/state/agent-mcp-go"),
    "skills_root": str(home / ".config/agent-mcp-go/skills"),
    "token_file": str(token_path),
    "public_url": "http://127.0.0.1:3100",
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

The server does not expand `~` or `$HOME` inside JSON path strings. Shell expansion in the following command happens before AMG receives the path.

### 3. Start in the foreground

```sh
"$HOME/.local/bin/agent-mcp-go" --config "$HOME/.config/agent-mcp-go/config.json"
```

In another terminal:

```sh
curl -fsS http://127.0.0.1:3100/healthz
```

The response body is `ok` followed by a newline. Health is unauthenticated and does not establish MCP authentication or client compatibility.

### 4. Connect a client

Configure a client that supports Streamable HTTP and a static Authorization header:

| Setting | Value |
| --- | --- |
| URL | `http://127.0.0.1:3100/mcp` for a client on the same machine |
| Header name | `Authorization` |
| Header value | `Bearer <contents of your mcp-token file>` |

Use `/mcp/stateless` for clients that do not preserve `Mcp-Session-Id`. Both endpoints require the same Bearer token. AMG does not provide an OAuth flow.

For remote clients, supply your own HTTPS reverse proxy or tunnel, set `public_url` to its external origin, and forward `/mcp`, `/mcp/stateless`, and `/transfer/` as needed. AMG listens only on loopback and does not install or manage ingress. See [client and proxy integration](docs/integrations.md).

Ask the connected agent to inspect its environment and list the workspace. SDK/protocol compatibility does not guarantee a host's native cards, attachment import, or download UI; verify those in your chosen client.

## Build and verify

From the source tree with Go 1.25+:

```sh
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o agent-mcp-go .
```

Network tests are opt-in and require an explicit `AGENT_MCP_GO_CONFIG` path. They may execute commands or create temporary files on the configured server; use a disposable workspace. See [release verification](docs/releases/v1.1.0.md#installation-and-verification).

## Documentation

- [Configuration](docs/configuration.md): required fields, limits, paths, and service setup.
- [Integrations](docs/integrations.md): HTTP clients, optional Skills/shared MCP, and scope continuity.
- [Security](docs/security.md): OS permissions, token handling, transfer URLs, and concurrency limits.
- [Changelog](CHANGELOG.md) and [v1.1.0 release notes](docs/releases/v1.1.0.md).

## Acknowledgments and license

AMG is released under the [MIT License](LICENSE). [Codex Free](https://github.com/hypnguyen1209/codex-free) informed parts of the workspace tool behavior and documentation organization. The bundled review UI is adapted from Codex Free v1.9.1, copyright © 2026 hypnguyen1209, under MIT; it includes [Prism](https://prismjs.com/), copyright © 2012 Lea Verou, also under MIT. Their notices are retained in [the asset license](assets/CODEXFREE-REVIEW-LICENSE.txt).
