# Integrations

AMG is a standalone workspace MCP server. Skills, shared MCP routes, and the canonical long-term memory archive are optional connections to systems you already operate; AMG does not install, bundle, or own those systems.

| Responsibility | Owner |
| --- | --- |
| Files, commands, Git, transfers, review and task checkpoints | AMG workspace runtime |
| Skill inventory and Skill content | Existing Skills directory/manager, read on demand |
| Module inventory and tool/schema listing | Canonical shared MCP helper and gateway |
| Long-term memory archive | Canonical on-disk archive (Obsidian `brain/` pages first, then `raw/transcripts/agentsview/` for exact wording; former MemOS route is retired) |
| Shared native result delivery | AMG's small official SDK invocation adapter |

The default workflow loads a selected module rather than all upstream schemas. The three compatibility wrappers remain callable, but do not add another source of truth. Two HTTP modes, independent transfer URLs and direct command control remain core AMG capabilities. Optional integrations need no additional AMG daemon.

## HTTP clients and ingress

| Endpoint | Behavior |
| --- | --- |
| `/mcp` | Stateful Streamable HTTP using the official Go SDK and JSON responses. Clients preserve the negotiated `Mcp-Session-Id`. |
| `/mcp/stateless` | Stateless Streamable HTTP for clients that do not retain the session header. Uses JSON responses; standalone GET/SSE is unsupported. |
| `/healthz` | Unauthenticated GET, body `ok\n`. Indicates HTTP availability only. |
| `/transfer/…` | Short-lived capability URLs created by authenticated tools for file movement. |
| `/.well-known/oauth-protected-resource` | OAuth protected resource metadata (RFC 9728); the `/mcp` and `/mcp/stateless` variants name the same resource. |
| `/.well-known/oauth-authorization-server`, `/.well-known/openid-configuration` | Authorization server metadata (RFC 8414) and OIDC discovery, whichever a hosted client probes. |
| `/oauth/authorize` | Owner consent page; issues a single-use authorization code bound to the client and PKCE challenge. |
| `/oauth/token` | Authorization-code + PKCE exchange and refresh-token grant. |
| `/oauth/register` | Dynamic client registration (RFC 7591), enableable or disableable. |
| `/oauth/revoke` | Token revocation (RFC 7009). |

Both MCP endpoints require `Authorization: Bearer <token>`. The owner token from `token_file` always works. When `oauth.enabled` is set, access tokens issued by the built-in OAuth 2.1 server are accepted as well, and an unauthenticated request answers 401 with a `WWW-Authenticate: Bearer resource_metadata="…"` challenge so OAuth-capable clients can discover the flow. A client that can only send a static header keeps working unchanged.

For remote access, configure an operator-managed HTTPS reverse proxy or tunnel to the loopback listener. Set `public_url` to the external origin, for example `https://workspace.example.com`, without an MCP path. Preserve the external Host and Authorization headers, MCP session/protocol headers, methods, and response content types. AMG accepts the configured listener Host or exact configured public Host. A supplied browser Origin must equal `public_url` with its trailing slash removed, or one of the origins listed in `allowed_origins`.

Forward `/mcp`, `/mcp/stateless`, and `/transfer/` for the capabilities you use. Ensure transfer body sizes, timeouts, and buffering match the desired workload. Chunked upload uses PATCH and offsets; individual chunks are bounded at 16 MiB. No-clobber is the upload default; replacing an existing file requires explicit `overwrite=true`.

Proxy configuration and certificates are your responsibility. AMG neither provisions TLS nor installs/manages a tunnel.

## Gemini Web and OAuth clients

Hosted clients such as Google Gemini Web Connected Apps, Claude Web, and ChatGPT sign in through a browser and cannot attach a static `Authorization` header. AMG ships an OAuth 2.1 authorization server as an optional entry point for these hosts: it is disabled by default, and every field is documented in [configuration](configuration.md#oauth-authorization-server).

Point `public_url` at a stable HTTPS origin first, then enable OAuth and restart:

```json
{
  "public_url": "https://workspace.example.com",
  "oauth": {
    "enabled": true,
    "allow_dynamic_registration": true,
    "access_token_ttl_seconds": 2592000,
    "refresh_token_ttl_days": 90,
    "clients": []
  }
}
```

### Google Connected Apps MCP compatibility

Conclusion: the current implementation works as a custom MCP connection under the Google Connected Apps MCP integration standard. The table below checks each requirement against the v1.2.2 implementation and the nine checks in `scripts/verify-gemini-web.sh`.

| Requirement | Current implementation | Result |
| --- | --- | --- |
| Transport | Streamable HTTP from the official Go SDK on a single `/mcp` path; `initialize`, `tools/list`, and `tools/call` all work | Pass |
| Accept | POST requires an `Accept` header that lists both `application/json` and `text/event-stream`; either media type alone is rejected with 400. The server answers with JSON | Pass |
| Stateless GET | GET on `/mcp/stateless` returns 405; there is no standalone SSE stream | Pass |
| Tool annotations | All 40 tools declare `readOnlyHint`, `destructiveHint`, `openWorldHint`, and `idempotentHint` explicitly in `default` mode, so `tools/list` is complete without relying on implicit defaults. `trusted` mode overrides them to read-only, non-destructive, and not open-world for the owner's client. The two modes are independent and do not conflict | Pass |
| OAuth | OAuth 2.1 with PKCE (S256 only) and dynamic client registration; optional and disabled by default, enabled with `oauth.enabled`; capped at 128 clients | Pass |
| Metadata | RFC 9728 protected resource metadata and RFC 8414 authorization server metadata, with `/mcp` suffix variants | Pass |
| Redirects | Six Google callback patterns accepted by default: `*.googleusercontent.com`, `*.google.com`, `oauth-redirect.googleusercontent.com` plus its sandbox and test variants, and `gemini.google.com` | Pass |
| Refresh | Refresh tokens do not rotate; the same refresh token keeps minting access tokens | Pass |
| TLS and proxy | The reverse proxy terminates TLS, disables response buffering, and forwards auth and MCP headers; this deployment follows [HTTP clients and ingress](#http-clients-and-ingress) | Operator requirement |

### Connect Gemini Web

1. Open the custom MCP connection entry in Gemini Web, usually under Settings → Connected Apps; the exact label changes as Google revises the UI.
2. Set the URL to `https://agent.cosymart.top/mcp`, that is `public_url` plus `/mcp`; replace the host when the deployment uses another domain. The bare origin also works because it hits the same stateful entry point on the root path `/`. Never use an `/oauth` address.
3. Leave the client ID and client secret fields empty to use zero-credential dynamic registration.
4. The browser opens AMG's consent page; enter the owner passkey as described in the next section.
5. After approval the browser redirects back to Gemini and the connection is live. Access tokens last 30 days and refresh tokens 90 days by default; the client refreshes them without a new consent.
6. Verify by asking Gemini to call a read-only tool such as `clock_curr_time` or `get_environment`. A result means the path works; whether a single call still needs human confirmation is host policy, documented under [Gemini repeated tool confirmations](#gemini-repeated-tool-confirmations).

When a host does not keep `Mcp-Session-Id`, use `/mcp/stateless` as the connection URL; Gemini Web keeps the session, so use `/mcp`.

### Owner passkey on the consent page

This is where real setups most often get stuck. The consent page only accepts the local owner's MCP token. It does not ask for and does not validate a Gemini account password.

- The passkey is the token in `token_file`, by default `$HOME/.config/agent-mcp-go/mcp-token` (mode `0600`).
- Two forms are equivalent: the bare token, or the `Bearer <token>` form (the prefix is case-insensitive). Do not add quotes or line breaks.
- When approval fails, first confirm the file belongs to this deployment and is not a stale or rotated token from another instance.

### Zero-credential setup (dynamic registration)

With the client ID and secret fields empty, the client uses DCR: it probes `/.well-known/oauth-authorization-server` and `/.well-known/openid-configuration`, then `POST /oauth/register`s as a public client (`token_endpoint_auth_method: "none"`, PKCE) and receives a client ID of the form `amg_cid_…`. AMG keeps at most 128 dynamic clients and evicts the oldest registration first. No credentials need to be prepared in advance.

### Pre-registered Client ID / Client Secret

Some hosts require pre-provisioned credentials, including the Gemini client ID and client secret fields. Register the client in the `oauth.clients` array:

```json
{
  "client_id": "gemini-web",
  "client_name": "Gemini Web",
  "client_secret_sha256": "<64 lowercase hex characters: sha256(client_secret)>",
  "redirect_uris": [
    "https://oauth-redirect.googleusercontent.com/r/*",
    "https://oauth-redirect.googleusercontent.com/a/*"
  ]
}
```

`client_secret_sha256` is the lowercase hex SHA-256 of the secret; AMG stores only the digest. Paste the plaintext secret into the host's client secret field and `client_id` into the client ID field. `redirect_uris` must match the host's actual callback; an empty array falls back to the global `allowed_redirect_uris` patterns. A static client without a secret is valid and is treated as a public PKCE client.

### Consent and token lifetime

The consent page shows the client name, client ID, callback, and scopes. Approval requires the owner Bearer token as the authorization password; on success AMG returns a 303 redirect to the callback with a single-use authorization code valid for five minutes, bound to the client, `redirect_uri`, and PKCE `code_verifier`.

The access token lifetime comes from `access_token_ttl_seconds` (default 2592000 seconds) and the refresh token lifetime from `refresh_token_ttl_days` (default 90 days). Refresh tokens do not rotate and can mint access tokens repeatedly. The identity is recorded as `oauth:<client_id>`, so a stateful `Mcp-Session-Id` stays valid across a refresh; a different token gets 403 session user mismatch. Revoking a refresh token also revokes the access tokens it issued. Deleting `<state_dir>/oauth/state.json` while the service is stopped clears all grants and dynamic clients.

### Tool descriptions and naming rules

v1.2.2 tightened tool descriptions. It is both a Google review requirement and a condition for not being downranked after connection. New tools follow the same rules:

- A description states what the tool does and what it takes and returns. It must not contain behavior instructions such as "call X first", "always use Y", or "ignore previous instructions", and must not reference other tools or workflows.
- Do not use uppercase warning words such as `MUST`, `NEVER`, `ALWAYS`, or `IMPORTANT`; annotations and the host's confirmation flow carry risk information.
- Tool names are snake_case verb-object phrases with a single responsibility, such as `read_file` or `git_push`; the 40 tool names and input schemas stay unchanged.
- `title` is a descriptive two-to-four-word sentence-case phrase, such as `Read file` or `Git push`.
- Security annotations reflect real semantics in `default` mode; `trusted` is for private single-user deployments where the owner accepts the risk.

### Gemini repeated tool confirmations

OAuth consent decides whether the connection is allowed, not whether each tool call needs human approval. Gemini's repeated confirmations for write and sensitive operations are host policy, not a switch AMG can turn off. Official references: [custom app help](https://support.google.com/gemini/answer/17209137), [task permissions](https://support.google.com/gemini/answer/18766567), [task security](https://support.google.com/gemini/answer/17094507), and [Connected Apps MCP server optimization](https://developers.google.com/connected-apps/mcp/server-optimization) (annotations affect confirmation behavior).

- `default` annotation mode declares real semantics: tools that execute commands or write stdin, such as `exec_command` and `write_stdin`, are destructive and open-world with `readOnlyHint` false. The host decides whether to confirm each call.
- `trusted` mode declares every tool as `readOnlyHint: true`, `destructiveHint: false`, and `openWorldHint: false` in exchange for fewer confirmations. Set `annotations_mode` to `"trusted"` in `config.json`, or change it live with `settings_update`, then refresh the host's tool metadata. Keep `default` for multi-client or public deployments; `trusted` risk sits with the deployment owner.

Diagnosis order:

1. Confirm the confirmation preference is saved in Gemini under Settings → Connected Apps → App actions.
2. Open Gemini Apps Developer Mode and compare one `get_environment` call with two harmless `exec_command printf amg-probe` calls in the Event Log, checking whether tool schemas and calls reach AMG.
3. If the preference is saved but every call still prompts, treat unattended execution as unsupported by that host; 401 or token exchange failures are OAuth problems and are diagnosed separately.

Private custom connections are not bound by the public Connected Apps publication policy; public distribution must also satisfy Google's review requirements. This repository makes no claim about meeting that public policy.
## Client presentation and discovery

Use MCP tool introspection for the exact current names and schemas. The server supplies fresh instructions during initialization and discovery, using the configured text budget for the Skill catalog and project rules separately. Catalog entries retain the complete parsed description within the bounded YAML frontmatter; chosen Skill bodies are read separately. The structured Skill inventory has no fixed count. Clients may apply their own context limits.

The server includes workspace file, patch, Shell, Git, image, transfer, review, plan, note, and handover tools. The `remember` tool saves task-local notes and deletes a key when its value is empty; notes never touch shared long-term memory. Review UI resources and file resources are exposed through MCP. Their presentation depends on the host. Protocol and SDK tests do not establish support for every host's review cards, proprietary attachment parameters, native import/export, or download experience. Test the intended client workflow directly, especially ChatGPT cards and attachments.

## Native file inputs and client verification

`import_chatgpt_file` is the dedicated native attachment tool. Its `file` property is a file object containing the required `download_url` and `file_id` strings, with optional `mime_type` and `file_name`; the tool advertises `openai/fileParams: ["file"]`. The host supplies the authorized value when the user selects a file. Pass a new workspace destination in `path`; imports do not overwrite existing files.

`import_host_file` is the portable compatibility entry point. It still accepts a file object or a `staged:` reference prepared on the server, but it does not advertise that union as a native host file parameter. Do not fabricate download URLs, substitute a bare file ID, or assume the server can read a client's private filesystem.

For an existing connector, inspect the host's scanned tool inventory as well as the server's `tools/list`. If `import_chatgpt_file` is absent in the host while present on the server, use the host's tool-discovery refresh and collect its diagnostics if the mismatch remains. That observation alone does not establish whether the cause is cached metadata, host policy, or another connector restriction. Installing a new server binary does not prove that the host refreshed its catalog.

Verify native import with the actual selected attachment and check the saved byte count and SHA-256. Generic HTTP upload/download success proves the transfer engine separately. Review cards and host download UI require their own client test. MCP resource embedding is limited to 32 MiB; larger exports use the HTTP transfer URL and remain subject to host and proxy limits.

## Local Skills

Set `skills_root` to a directory laid out as:

```text
skills/
  example-skill/
    SKILL.md
    references/
      details.md
```

Discovery examines immediate child directories that contain `SKILL.md`. The directory name is the Skill identifier; a YAML frontmatter `description` supplies the catalog description. Use `skills_list` to discover and filter by a case-insensitive literal substring of the name or catalog description, then `skills_read` with `name` and an optional Skill-relative `resource` to read the body or supporting documents. Text reads default to the configured byte budget rather than a fixed line count; supply `offset` and positive `limit` for a shorter page. A continuation marker indicates more lines; an over-budget single line is rejected explicitly. Support file discovery uses the remaining text budget. Resource paths and resolved symlinks cannot escape the selected Skill directory.

AMG reads this existing directory on demand. It does not create a second registry or install a Skill manager. If `skills_root` is unset or unavailable, core tools still run and initialization reports that Skills are unavailable; Skill tools can report an error. An empty readable directory gives an empty catalog.

## Shared MCP CLI contract

`shared_mcp_bin` is an external executable path, not an HTTP endpoint or a shell command string. For each helper call AMG starts that executable **without command-line arguments**, inherits the execution environment, connects with the official SDK's MCP stdio transport, and imposes a 120-second timeout. The executable must implement these MCP tools:

| Helper tool | Arguments sent by AMG |
| --- | --- |
| `mcp_list_routes` | `{}` |
| `mcp_list_tools` | `{"server":"<route URL>"}`, optionally `tool_name`; catalog enumeration can also send `timeout_seconds: 30` |
| `mcp_call_json` | `{"server":"<route URL>","tool":"<name>","arguments_json":"<JSON object text>"}` |

Route discovery must yield a JSON object with a `routes` array whose entries contain `name`, `path`, and `url`. Entries without a name or URL are skipped. Source IDs are derived by removing the `/mcp/` prefix from `path`; source lookup accepts that ID or route name. Tool enumeration yields `{"tools":[<MCP tool schema objects>]}`. Exact lookup also accepts `{"tool":<schema>}` or a standalone schema for older helpers.

AMG first uses a helper result's structured content, otherwise its first text block. JSON may be returned directly or inside an `output` string wrapper. The helper-facing `shared_mcp_*` tools return this text representation; they do not preserve every upstream content block.

The exposed helper wrappers remain for existing consumers; new tasks use the module-first interface below. `shared_mcp_routes` and `shared_mcp_tools` retain their raw helper envelopes, while `shared_mcp_call` retains its text result contract. They are not alternative registries. Retire a compatibility entry only after its consumers have migrated; absence from a local code search does not establish absence from remote clients.

- `shared_mcp_routes`: calls `mcp_list_routes`.
- `shared_mcp_tools`: calls `mcp_list_tools` for one selected URL and optional exact tool.
- `shared_mcp_call`: validates that `arguments_json` is a JSON object string (default `{}`), then calls `mcp_call_json`.

For new tasks, use the module-first route interface:

1. `mcp_list_sources` reads current module identities from the canonical helper. It does not initialize target modules. A query matches published module names, IDs and descriptions literally. Current route metadata supplies names, paths and URLs; no purpose table or tool count is invented. An omitted `toolCount` means unknown, not zero. Choose the module using the task and its existing owner Skill.
2. `mcp_search_tools` with `source` and no `query` lists that module's tool summaries. A query filters names and descriptions literally. Only that module is enumerated; unrelated slow or broken modules are not touched. All summaries are returned by default; an explicit positive `limit` narrows results. `total` describes matches before that limit, not the health of other modules.
3. `mcp_get_tool` asks the helper for the selected `tool_name` when its exact schema is unknown. It validates the returned tool identity and non-null `inputSchema`. A compact catalog is a summary, never an exact schema. Schema and annotation data remain upstream-owned.
4. `mcp_call_tool` invokes a known source/tool directly without a catalog preflight. Its SDK connection preserves native MCP result blocks, `isError`, structured content and result metadata.

AMG maintains no global schema index, TTL cache, background catalog refresher, BM25 model or bilingual business dictionary. Module additions/removals and same-URL schema changes are observed on the next explicit discovery or schema request; no AMG rewrite or restart is required. A running agent's already loaded context is not replaced in place, and this does not promise client notification support.

Unscoped `mcp_search_tools(query)` remains an optional literal-name/description fallback for an unknown module. It is not semantic routing and is not the default. This fallback reads canonical helper catalogs with the hot-adjustable `mcp_scan_workers` and `mcp_discovery_timeout_seconds` resource bounds. Route discovery and helper shutdown are additional operations, not part of a total method latency guarantee. Omitted/zero `limit` returns all matches; a positive value narrows the result. Negative values are rejected. `score` is a compatibility field, not a relevance estimate. `force_refresh` is accepted for compatibility; directory reads are already request-local and fresh.

Partial fallback results set `incomplete: true`, `unavailableSources`, and safe per-source `sourceFailures` for this request. Empty catalogs are distinct from failed reads. Results are not retained for later requests, so failed or removed modules cannot reappear from an old index; `stale` remains false or omitted. Upstream errors do not establish the cause or health of other modules. Selected-module failures return an error for that module and do not launch a global search.

Directory listing and exact schema retrieval use the canonical helper's existing pagination and output limits. Large catalogs may contain only names and abbreviated descriptions; request an individual exact schema on demand. AMG does not duplicate the helper's inventory, pagination protocol or size policy.

`mcp_call_tool` discovers the selected route through the helper, then connects **directly to its URL** using the official Go SDK's Streamable HTTP client, with standalone SSE disabled and a 120-second timeout. It returns native MCP content blocks, structured results, and metadata. The direct call does not go through `mcp_call_json` and does not add custom upstream Authorization headers; discovered routes must be reachable and usable by this process with that transport.

An empty `shared_mcp_bin` makes these optional integration calls report `shared_mcp_bin not configured`; workspace tools remain available. AMG includes no shared MCP CLI, gateway, route inventory, credentials, or memory service.

## Task continuity and memory

Task state resolves in this order:

1. Explicit tool `scope`.
2. ChatGPT tool metadata `openai/session`, when supplied.
3. Stateful MCP session identity.

These use separate namespaces, so identical strings in different identity sources do not collide. Without any usable identity, task-state tools request an explicit scope. A new or empty task never imports another scope's records or legacy flat state.

To resume across reconnects or different clients, pass the same explicit scope, such as `project-maintenance-2026-11`, to plan, note, review, and handover calls. Saved plans, notes, and recorded project paths can persist under that scope. `get_agent_brief` restores that task's saved context; it does not restore chat transcripts or an active Shell process. Shell sessions have their own handles and lifecycle and end on server restart. Stateful Shell handles belong to the MCP session; stateless continuation relies on an unpredictable numeric handle.

Scopes provide logical task separation within one trusted OS account. They are not an ACL or a multi-user security boundary. Task `remember`/`recall` is scratch state. For durable long-term memory, read the canonical long-term archive — the Obsidian `brain/` pages, then `raw/transcripts/agentsview/` for exact wording; AMG bundles no memory service and exposes no memory route. The former MemOS recall layer was retired on 2026-11-19, so `/mcp/memos` is gone by design and is not a fallback.

## Concurrent agents on one server

Task state is written under a per-scope lock, keyed by the resolved state file. Two agents working in the same scope are serialized, so a plan or note file is never written twice at once, while agents in different scopes proceed fully in parallel. A canceled caller releases its lock and never blocks another agent.

Give every concurrent agent its own explicit `scope` value, and give every agent that edits the same repository its own `git worktree` checkout:

```sh
git -C /path/to/repository worktree add /path/to/worktrees/agent-task-a -b task/agent-a
```

Pass that checkout path as the `workdir` argument of `run_command`, or as the `directory` argument of the Git tools. Relative paths are resolved against `default_workdir`, and `work_root` stays the access boundary. Keep shared production services, deployment switches, and configuration files single-writer; an isolated worktree does not isolate a shared service.

## Ephemeral session state

Task state of an anonymous MCP session is disposable and expires after 30 days. The server marks such a scope directory, removes it when it is untouched for that long, and sweeps expired scopes at startup and at most once an hour afterwards, so a long-running daemon keeps cleaning up without a restart. Explicit scopes and ChatGPT conversation scopes are durable and are never removed by that sweep. Pass an explicit `scope` for work that must survive longer than 30 days.

## Reconnection and bounded inspection

If a client loses its tool list, reconnect that client and initialize/list again. AMG serves the current catalog on the new connection; it cannot replace the client's stored UI context. Reuse an explicit task scope to restore saved notes and plans across sessions. Reconnection does not replay a previous tool call. After a lost write response, inspect the affected state before deciding whether another write is needed.

Independent read-only checks may be grouped by a client's existing parallel-call support with a bounded fan-out. Keep writes and dependent operations ordered. AMG does not add a batch tool, reconnect daemon, heartbeat, route-purpose map or shared-MCP tool catalog cache. Module purpose is passed through only when supplied by the shared owner; compact tool summaries remain the default before loading a selected exact schema.
