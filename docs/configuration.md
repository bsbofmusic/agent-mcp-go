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
| `allowed_origins` | Optional list of extra browser or desktop client origins that may carry authenticated MCP requests. A non-empty list also enables the MCP CORS preflight and response headers. See [client origins](#client-origins) and [CORS behavior](#cors-behavior-and-browser-preflight). |
| `oauth` | Optional OAuth 2.1 authorization server for hosted MCP clients. See [OAuth authorization server](#oauth-authorization-server). |
| `skills_root` | Optional existing directory containing immediate child Skill directories, each with `SKILL.md`. An empty directory is sufficient for a standalone installation. |
| `shared_mcp_bin` | Optional absolute path to an external MCP stdio CLI implementing the interface described in [integrations](integrations.md#shared-mcp-cli-contract). Leave empty when unavailable. |
| `executable_dirs` | Optional list of absolute executable directories. Entries must not contain the OS path-list separator (`:` on Linux). |
| `bootstrap_files` | Default `[]`. List of non-empty, NUL-free absolute paths lexically inside `work_root`. Eligible files are inlined into the initial agent instructions under the `max_output_bytes` UTF-8 byte cap for the rendered section. See [bootstrap context](#bootstrap-context-and-annotations). |
| `annotations_mode` | Default `default`. `default` keeps per-tool MCP safety annotations. `trusted` advertises all tools on `tools/list` with `readOnlyHint: true`, `destructiveHint: false`, and `openWorldHint: false`. Hosts control their confirmation policies. |
| `max_file_bytes` | Default 300 MiB; 1 MiB–1 GiB. File/transfer budget. |
| `max_output_bytes` | Default 512 KiB; 1 KiB–4 MiB. Text, Skill/project sections, bootstrap context and task state budget. |
| `command_timeout_seconds` | Default 60; 1–3,600. Execution/Git/upstream tool-call timeout. |
| `default_yield_ms` | Default `10000`. Range 250–60,000. Wait window for `exec_command` and `write_stdin` when `yield_time_ms` is omitted, zero, or negative. Must be at most `max_yield_ms`. |
| `max_yield_ms` | Default `30000`. Range 1,000–120,000. Upper bound for every `exec_command` and `write_stdin` wait window. Must be at least `default_yield_ms`. |
| `max_shell_sessions` | Default 8; 1–32. Concurrent running Shell sessions. |
| `max_search_entries` | Default 50,000; 1–1,000,000. Search traversal/node budget. |
| `max_search_depth` | Default 64; 1–256. Directory traversal depth budget. |
| `max_search_file_bytes` | Default 4 MiB; 1 KiB–16 MiB. Maximum file scanned by native grep; oversized files are reported. |
| `max_image_bytes` | Default 5 MiB; 1 KiB–32 MiB. Native image content size budget. |
| `mcp_discovery_timeout_seconds` | Default 45; 1–120 (canonical helper range). Route directory/aggregate scan deadline. |
| `mcp_scan_workers` | Default 8; 1–32. Concurrent modules for unscoped search fallback. |
| `ignore` | Optional search/listing ignore configuration; see below. |

Tuning keys may be omitted to use defaults; explicit values must be in range. A 300 MiB `max_file_bytes` value is `314572800`. External helper, proxy and client limits can be lower. The text budget is not a global wire-message limit: Skill structured inventory, native MCP results and separate bootstrap sections have their own representations.

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

### CORS behavior and browser preflight

`allowed_origins` decides two things at once: which browser origins may carry authenticated MCP requests, and whether CORS response headers are emitted. Both behaviors share one list, so there is no half-enabled state where a request is allowed but the headers are missing.

When `allowed_origins` is empty (the default), behavior matches v1.2.1: only the `public_url` origin and requests without an `Origin` header are accepted, the MCP entry points emit no `Access-Control-*` headers at all, and browser cross-origin calls are blocked by the browser.

When `allowed_origins` is non-empty, the MCP entry points enable CORS:

- `OPTIONS /mcp`, `OPTIONS /mcp/stateless`, and `OPTIONS /` are handled as preflight requests before Bearer authentication, because browser preflights carry no `Authorization`. A whitelisted origin gets `204 No Content` with `Access-Control-Allow-Origin: <origin>`, `Vary: Origin`, `Access-Control-Allow-Methods: POST, GET, DELETE, OPTIONS`, `Access-Control-Allow-Headers: Content-Type, Authorization, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID`, and `Access-Control-Max-Age: 600`.
- A preflight whose origin is not whitelisted, missing, or repeated gets `403` with the body `origin denied` and no `Access-Control-*` headers. An `OPTIONS` request without an `Origin` header is not a browser preflight and is rejected the same way; v1.2.1 answered it with `401` on these paths, so monitoring that treats that status as an authentication signal should expect `403`.
- Actual requests from a whitelisted origin get `Access-Control-Allow-Origin: <origin>` and `Vary: Origin` on the response, plus `Access-Control-Expose-Headers: Mcp-Session-Id, WWW-Authenticate`, so the browser can read the session ID and the 401 challenge; the request still passes through normal authentication and returns 401 without a valid Bearer token.
- `Access-Control-Allow-Credentials` is never sent. Browsers use the `Authorization` header, not cookies.

Other endpoints are unaffected: `/healthz` and `/transfer/` behave as before, and the OAuth endpoints (`/oauth/*` and the well-known metadata) keep their own CORS with `Access-Control-Allow-Origin: *` so hosted clients can read metadata.

A typical hosted-client configuration lists fixed origins explicitly:

```json
{
  "allowed_origins": ["https://gemini.google.com", "https://claude.ai", "https://chatgpt.com"]
}
```

Do not use `*` instead of a fixed list; `*` removes the browser-side origin restriction.

Troubleshooting: on a browser CORS error, first confirm the preflight returns 204 with `Access-Control-Allow-Origin`; if the preflight returns 401, `allowed_origins` is empty (the CORS layer is not enabled) or the origin is not whitelisted.
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

Commands inherit the server process environment. Executable lookup keeps absolute entries from its `PATH`, then adds `executable_dirs` and standard user locations such as the user's `.local/bin` and `.nix-profile/bin`, removing duplicates. It never implicitly adds the current directory. `run_command` resolves a single executable name from this PATH and passes argv directly without shell parsing; no fixed list of programs is needed. The legacy `run_command_allowed` field is empty, meaning unrestricted PATH lookup within the authenticated agent account. Use `exec_command` for explicit shell syntax. Check `command -v <program>` through the remote Shell when a service environment differs from a login shell.

AMG does not install development tools. Install Git, language runtimes, and other commands under the OS account's normal administration process.

## Ignore options

The `ignore` object accepts `use_gitignore`, `use_default_patterns`, and `custom_patterns`. The first two are optional booleans; enabled behavior is the default when omitted. Custom patterns are a list of glob-style strings. The parser caps custom patterns at 4,096 entries and 1 MiB in aggregate. Search traversal always prunes `.git` and `node_modules` directories.

Ignore rules control discovery and output; they are not access-control rules. Explicit tools and Shell remain governed by their own path and OS permissions.

## Settings and restart behavior

`settings_get` and `settings_update` cover all 14 live settings in the field table: the 12 numeric resource and yield settings, `annotations_mode`, and `bootstrap_files`. Both tools report the live values, defaults, and ranges or allowed values. For `bootstrap_files`, the report includes both the count and the paths.

`settings_update` accepts a partial JSON object such as `{"max_output_bytes":1048576}`. It validates the merged live settings before atomically persisting them to the selected file. Omitted fields retain their values, and a rejected update leaves the previous settings in effect on disk and in memory. Updated values take effect without a restart. In-flight calls may finish using their earlier budget. Lowering Shell concurrency does not terminate running sessions. Structural fields such as the listener, roots, ingress origin, and `oauth` require a config edit and restart. Restart after rotating the startup Bearer token, and update authorized clients.

The next `get_agent_brief` / initialize response uses the live `bootstrap_files` list. Each `tools/list` request uses the live `annotations_mode`, so a host that refreshes tool metadata sees the current hints.

For `exec_command` and `write_stdin`, `waitContext` uses `default_yield_ms` when `yield_time_ms` is omitted or ≤ 0. It raises a positive value below 250 ms to 250 ms and clamps the resulting wait window at `max_yield_ms`. Each call reads the live bounds, including calls involving an already running Shell session. A command may complete before its wait window expires.

## Bootstrap context and annotations

`bootstrap_files` inlines a few small, always-needed files into the initial instructions so a new agent starts oriented without extra round trips:

```json
{
  "work_root": "/home/amg/workspace",
  "bootstrap_files": [
    "/home/amg/workspace/AGENTS.md",
    "/home/amg/workspace/project-map.md"
  ]
}
```

At config load and `settings_update` time, AMG trims each entry, checks that it is a non-empty, NUL-free absolute path lexically contained in `work_root`, rejects entries whose cleaned path has leading or trailing whitespace, and stores the cleaned absolute path. A file may be absent at either point. At read time, AMG opens the workspace with `os.OpenRoot(work_root)`, opens the entry relative to that root with `O_NONBLOCK`, and checks that the opened file is regular. The rooted open enforces filesystem containment even when a path uses symlinks.

The entire rendered bootstrap section, including its headings, file bodies, truncation notices, and unavailable notices, is at most `max_output_bytes` UTF-8 bytes after normalization and after decoding the MCP JSON response. A body that exceeds the remaining budget is truncated and gets a `read_file` pointer as space allows. Missing, unreadable, and ineligible files are marked unavailable within that same budget. AMG stops reading entries once the section is full. Keep the list limited to context needed at startup.

`annotations_mode` selects how the server advertises MCP tool safety hints:

| Value | Effect |
| --- | --- |
| `default` (or omitted) | Accurate per-tool hints. Shell, stdin and arbitrary execution stay destructive and open-world, and are not read-only. |
| `trusted` | Every tool in `tools/list` uses `readOnlyHint: true`, `destructiveHint: false`, and `openWorldHint: false`. Hosts that use these hints to gate confirmations may show fewer prompts. |

Host policy controls any mandatory checks that apply independently of MCP annotations. AMG continues to enforce its authorization and workspace rules in both modes. Use `trusted` only for a private deployment with one trusted owner who accepts the risk of fewer confirmations. Keep `default` for shared, multi-client or publicly reachable installations. See [Gemini confirmation notes](integrations.md#gemini-repeated-tool-confirmations).

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
