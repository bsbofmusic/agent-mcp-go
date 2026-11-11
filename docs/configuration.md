# Configuration

AMG reads one JSON configuration selected by `--config /absolute/path/config.json` or `AGENT_MCP_GO_CONFIG`. An explicit `--config` takes precedence. There is no automatic home-directory configuration lookup.

Start from [config.example.json](../config.example.json) and follow the [README setup](../README.md#quick-start). The example's `/home/amg` paths are placeholders. Use absolute paths for all configured filesystem locations; JSON strings do not expand `~`, `$HOME`, or other environment variables.

## Fields

| Key | Meaning and constraints |
| --- | --- |
| `listen` | Required. Loopback address beginning with `127.0.0.1:` or `[::1]:`, for example `127.0.0.1:3100`. Public listeners are refused. |
| `work_root` | Required absolute path. Root for workspace file tools; create it before use. It can contain several projects. |
| `default_workdir` | Optional absolute path inside `work_root` that must already exist. It is the starting directory for relative paths and for tools that take no explicit directory. When omitted, `work_root` is used. |
| `state_dir` | Required absolute path. Private task state and temporary export snapshots. Created on startup when absent. Keep outside the workspace. |
| `token_file` | Required path to the static Bearer token. Use an absolute path outside the workspace, mode `0600`, containing at least 32 characters after trimming whitespace. Startup rejects group/other permissions and short tokens. |
| `public_url` | External origin used for generated transfer URLs and HTTP Host/Origin checks, without `/mcp`; for local use, `http://127.0.0.1:3100`. For remote use, set your own HTTPS origin. |
| `allowed_origins` | Optional list of extra browser or desktop client origins that may carry authenticated MCP requests. See [client origins](#client-origins). |
| `oauth` | Optional OAuth 2.1 authorization server for hosted MCP clients. See [OAuth authorization server](#oauth-authorization-server). |
| `skills_root` | Optional existing directory containing immediate child Skill directories, each with `SKILL.md`. An empty directory is sufficient for a standalone installation. |
| `shared_mcp_bin` | Optional absolute path to an external MCP stdio CLI implementing the interface described in [integrations](integrations.md#shared-mcp-cli-contract). Leave empty when unavailable. |
| `executable_dirs` | Optional list of absolute executable directories. Entries must not contain the OS path-list separator (`:` on Linux). |
| `max_file_bytes` | Required integer, **1,048,576–1,073,741,824** (1 MiB–1 GiB). Sets the configured transfer/file limit. |
| `max_output_bytes` | Required integer, **1,024–4,194,304** (1 KiB–4 MiB). Bounds tool output. |
| `command_timeout_seconds` | Required integer, **1–3,600**. Command runtime limit. |
| `ignore` | Optional search/listing ignore configuration; see below. |

Required tuning values must be present and positive within these ranges; zero is invalid. A 300 MiB `max_file_bytes` value is `314572800`. External proxy and client limits can be lower than AMG's configured limit.

The current parser enforces absolute `work_root` and `state_dir`; use an absolute `token_file` as a deployment requirement too. Keep configuration and token files owner-only (`0600`) and private parent directories `0700`.

## Working directory

`default_workdir` selects the directory that tools use when the caller supplies no explicit path. It must be an existing directory inside `work_root`; `work_root` remains the security boundary. When the field is omitted, `work_root` itself is the default.

Relative paths are resolved against that directory. `run_command` accepts an optional `workdir` argument that overrides it for one call:

| Call | Working directory |
| --- | --- |
| `run_command` without `workdir` | `default_workdir` |
| `run_command` with a relative `workdir` | `default_workdir` plus that relative path |
| `run_command` with an absolute `workdir` | that directory, when it stays inside `work_root` |
| `run_command` with a missing path or a regular file | refused |

The same default applies to the file tools, the Git tools, and the project-rules lookup. A `workdir` outside `work_root` is refused.

## Client origins

`public_url` is the origin that the server expects from a browser client. When `allowed_origins` is omitted or empty, only that origin is accepted, and a request without an `Origin` header is accepted as well.

`allowed_origins` adds explicit client origins, for example a hosted web client or a desktop application:

```json
{
  "allowed_origins": ["https://client.example.com", "app://obsidian.md"]
}
```

Each entry is either `*` or a bare origin: a scheme and a host, without path, query, fragment, credentials, or trailing slash. Desktop clients that use a custom scheme, such as `app://`, `vscode-webview://` or `tauri://localhost`, are accepted when scheme and host are present. Startup rejects malformed entries and empty strings.

The entry `*` allows every origin and therefore removes the browser-side origin restriction. Use it only when another control, such as an authenticated private network or a client that cannot send an `Origin` header, keeps the deployment safe.

The origin check supplements Bearer authentication; it never replaces it. A request without a valid Bearer token is rejected as unauthorized before any origin is considered. See the [security model](security.md#authentication-and-network-exposure).

## OAuth authorization server

Hosted MCP clients that cannot send a static header — Google Gemini Web Connected Apps, Claude Web, ChatGPT — can authorize through AMG itself. The server is off by default:

```json
{
  "public_url": "https://amg.example.com",
  "oauth": {
    "enabled": true,
    "allow_dynamic_registration": true,
    "access_token_ttl_seconds": 2592000,
    "refresh_token_ttl_days": 90,
    "clients": [
      {
        "client_id": "gemini-web",
        "client_name": "Gemini Web",
        "client_secret_sha256": "<64 hex characters>",
        "redirect_uris": ["https://*.googleusercontent.com/*"]
      }
    ]
  }
}
```

| Key | Meaning and constraints |
| --- | --- |
| `enabled` | Turns the OAuth endpoints on. `public_url` must then be a valid `http://` or `https://` base URL without query or fragment; a trailing `/` is removed. |
| `allowed_redirect_uris` | Optional redirect URI patterns. When empty, AMG supplies defaults for Google Gemini Web (`https://*.googleusercontent.com/*`, `https://*.google.com/*`, `https://gemini.google.com/*`), `https://claude.ai/*`, `https://*.anthropic.com/*`, `https://chatgpt.com/*`, `https://*.openai.com/*`, `http://127.0.0.1:*`, and `http://localhost:*`. Set `["https://*"]` to explicitly opt into any clean HTTPS callback. |
| `allow_dynamic_registration` | Optional, default `true`. When `false`, `/oauth/register` answers 403 and every client must be listed in `clients`. |
| `access_token_ttl_seconds` | Optional, default `2592000` (30 days), allowed **60–31,536,000**. |
| `refresh_token_ttl_days` | Optional, default `90`, allowed **1–365**. |
| `clients` | Optional pre-registered clients. `client_id` must be unique and non-empty; `client_secret_sha256` is the SHA-256 of the client secret in hex; `redirect_uris` further restricts that client. |

A redirect URI pattern is an exact URI, a prefix wildcard ending in `/*`, or the loopback forms `http://127.0.0.1:*` and `http://localhost:*`. `https` is required everywhere except loopback. Fragments, credentials, and `..` path segments are rejected, and an unmatched `redirect_uri` produces a 400 instead of a redirect to the attacker.

OAuth state lives in `<state_dir>/oauth/state.json` (mode `0600`). It stores only SHA-256 digests of access tokens, refresh tokens and client secrets — never plaintext credentials. Access tokens, refresh tokens and dynamically registered clients are bounded (512/256/128) and expired material is swept on every request; the authorization code and consent ticket caches are in memory, single-use, and capped at 256 entries.

The owner Bearer token remains the consent password: the consent page asks for it before any code is issued, and it always keeps full access. Refresh tokens do not rotate — the token endpoint returns the same refresh token with a new access token for the same client, so a hosted client can refresh hourly without re-consent. Revoking a refresh token also revokes the access tokens minted from it. See the [Gemini Web setup](integrations.md#gemini-web-and-oauth-clients).

## Executable lookup

Commands inherit the server process environment. Executable lookup keeps absolute entries from its `PATH`, then adds `executable_dirs` and standard user locations such as the user's `.local/bin` and `.nix-profile/bin`, removing duplicates. It never implicitly adds the current directory. Check `command -v <program>` through the remote Shell when a service environment differs from a login shell.

AMG does not install development tools. Install Git, language runtimes, and other commands under the OS account's normal administration process.

## Ignore options

The `ignore` object accepts `use_gitignore`, `use_default_patterns`, and `custom_patterns`. The first two are optional booleans; enabled behavior is the default when omitted. Custom patterns are a list of glob-style strings. The parser caps custom patterns at 4,096 entries and 1 MiB in aggregate. Search traversal always prunes `.git` and `node_modules` directories.

Ignore rules control discovery and output; they are not access-control rules. Explicit tools and Shell remain governed by their own path and OS permissions.

## Settings and restart behavior

The settings tool can update the three tuning values and persist them to the selected configuration file. Structural fields such as listener, roots, ingress origin, and the `oauth` object should be changed in the file and applied by restarting AMG. The Bearer token is read at startup; restart after rotation and update authorized clients.

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
