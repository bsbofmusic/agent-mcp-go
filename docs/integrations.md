# Integrations

AMG is a standalone workspace MCP server. Skills, shared MCP routes, and semantic memory are optional connections to systems you already operate; AMG does not install, bundle, or own those systems.

## HTTP clients and ingress

| Endpoint | Behavior |
| --- | --- |
| `/mcp` | Stateful Streamable HTTP using the official Go SDK and JSON responses. Clients preserve the negotiated `Mcp-Session-Id`. |
| `/mcp/stateless` | Stateless Streamable HTTP for clients that do not retain the session header. Uses JSON responses; standalone GET/SSE is unsupported. |
| `/healthz` | Unauthenticated GET, body `ok\n`. Indicates HTTP availability only. |
| `/transfer/…` | Short-lived capability URLs created by authenticated tools for file movement. |

Both MCP endpoints require `Authorization: Bearer <token>`. There is no OAuth authorization server, browser login, or automatic token exchange. Choose a client that can send a static Authorization header.

For remote access, configure an operator-managed HTTPS reverse proxy or tunnel to the loopback listener. Set `public_url` to the external origin, for example `https://workspace.example.com`, without an MCP path. Preserve the external Host and Authorization headers, MCP session/protocol headers, methods, and response content types. AMG accepts the configured listener Host or exact configured public Host. A supplied browser Origin must equal `public_url` with its trailing slash removed.

Forward `/mcp`, `/mcp/stateless`, and `/transfer/` for the capabilities you use. Ensure transfer body sizes, timeouts, and buffering match the desired workload. Chunked upload uses PATCH and offsets; individual chunks are bounded at 16 MiB. No-clobber is the upload default; replacing an existing file requires explicit `overwrite=true`.

Proxy configuration and certificates are your responsibility. AMG neither provisions TLS nor installs/manages a tunnel.

## Client presentation and discovery

Use MCP tool introspection for the exact current names and schemas. The server supplies fresh instructions during initialization and discovery, bounded at 24 KiB, with a Skills catalog preview bounded at 13 KiB. Catalog entries are descriptions; chosen Skill bodies are read separately.

The server includes workspace file, patch, Shell, Git, image, transfer, review, plan, note, and handover tools. Review UI resources and file resources are exposed through MCP. Their presentation depends on the host. Protocol and SDK tests do not establish support for every host's review cards, proprietary attachment parameters, native import/export, or download experience. Test the intended client workflow directly, especially ChatGPT cards and attachments.

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

Discovery examines immediate child directories that contain `SKILL.md`. The directory name is the Skill identifier; a YAML frontmatter `description` supplies the catalog description. Use `skills_list` to discover and filter, then `skills_read` with `name` and an optional Skill-relative `resource` to read the body or supporting documents. Reads are bounded and support pagination. Resource paths and resolved symlinks cannot escape the selected Skill directory.

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

The exposed helper wrappers are:

- `shared_mcp_routes`: calls `mcp_list_routes`.
- `shared_mcp_tools`: calls `mcp_list_tools` for one selected URL and optional exact tool.
- `shared_mcp_call`: validates that `arguments_json` is a JSON object string (default `{}`), then calls `mcp_call_json`.

For the higher-level route interface, use `mcp_list_sources`, `mcp_search_tools`, `mcp_get_tool`, then `mcp_call_tool`. The helper discovers current routes. Catalog enumeration connects to each discovered URL using the official Go SDK's Streamable HTTP client with standalone SSE disabled, matching the transport and access requirements of `mcp_call_tool`; it does not add custom upstream Authorization headers. Full tool schemas enter the existing in-process catalog rather than a helper's abbreviated catalog response. The catalog lasts two minutes normally, 15 seconds after partial failure. At most eight routes are scanned concurrently, with a 30-second per-route budget and a 45-second scan-phase budget. Route discovery and SDK session cleanup are separate operations; these budgets are not a total method latency guarantee. Route changes invalidate the catalog and `force_refresh=true` bypasses it. Search limits default to 10 and accept 1–50.

Each source catalog is bounded at 100 pages, 10,000 tools, and 16 MiB of serialized tool metadata. HTTP response bodies and SSE events are also bounded at 16 MiB. Repeated pagination cursors fail the scan rather than looping indefinitely.

A partial result sets `incomplete: true` and lists `unavailableSources`, including routes that could not start before the scan budget expired. `sourceFailures` records each failed source's safe stage, error category, and elapsed milliseconds without exposing raw errors or URLs. Missing catalog entries cannot be ranked; a search hit rate must therefore be reported alongside source coverage. These fields describe this observation window, not a diagnosis of an upstream service. A fully enumerated empty catalog is distinct from a failed enumeration. Expired results returned during background refresh, or old rows retained after a total outage, set `stale: true` and `incomplete: true`; stale rows cannot supply exact schemas.

Chinese search uses Han bigrams and general bilingual query synonyms, separates adjacent Latin/Chinese text, and lowers the weight of generic search verbs. It ranks the live tool names, descriptions, and supported schema text; it does not assign fixed route owners to business topics. Ambiguous queries can still return several sources, and no translation table substitutes for an unavailable catalog.

When a fresh catalog for the current route signature contains a successfully enumerated source, source-scoped search and exact schema lookup reuse it. Otherwise they query the selected source directly; explicit enumeration retains its separate timeout. A source-only cache miss does not create a second catalog or persistent cache, and may require another lookup if no matching catalog snapshot becomes available.

`mcp_call_tool` discovers the selected route through the helper, then connects **directly to its URL** using the official Go SDK's Streamable HTTP client, with standalone SSE disabled and a 120-second timeout. It returns native MCP content blocks, structured results, and metadata. The direct call does not go through `mcp_call_json` and does not add custom upstream Authorization headers; discovered routes must be reachable and usable by this process with that transport.

An empty `shared_mcp_bin` makes these optional integration calls report `shared_mcp_bin not configured`; workspace tools remain available. AMG includes no shared MCP CLI, gateway, route inventory, credentials, or memory service.

## Task continuity and memory

Task state resolves in this order:

1. Explicit tool `scope`.
2. ChatGPT tool metadata `openai/session`, when supplied.
3. Stateful MCP session identity.

These use separate namespaces, so identical strings in different identity sources do not collide. Without any usable identity, task-state tools request an explicit scope. A new or empty task never imports another scope's records or legacy flat state.

To resume across reconnects or different clients, pass the same explicit scope, such as `project-maintenance-2026-11`, to plan, note, review, and handover calls. Saved plans, notes, and recorded project paths can persist under that scope. `get_agent_brief` restores that task's saved context; it does not restore chat transcripts or an active Shell process. Shell sessions have their own handles and lifecycle and end on server restart. Stateful Shell handles belong to the MCP session; stateless continuation relies on an unpredictable numeric handle.

Scopes provide logical task separation within one trusted OS account. They are not an ACL or a multi-user security boundary. Task `remember`/`recall` is scratch state. For semantic long-term memory, discover and use an existing MemOS route through the shared interface; no MemOS instance is bundled.
