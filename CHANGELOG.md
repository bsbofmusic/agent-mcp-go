# Changelog

## 1.1.4

### Gemini Web Connected Apps & Remote OAuth Discovery

- Mount the authenticated stateful MCP handler at `/{$}` alongside `/mcp`, so Connected Apps origin-root probes (`HEAD /` and `POST /`) receive an RFC 9728 `401` `WWW-Authenticate` challenge rather than `404` and either `https://<host>` or `https://<host>/mcp` can be entered as the MCP URL.
- Serve `HTTP HEAD` (with matching headers and empty body) on all `/.well-known/oauth-protected-resource*`, `/.well-known/oauth-authorization-server*`, and `/.well-known/openid-configuration*` endpoints, and add `/.well-known/openid-configuration/mcp`.
- Support `https://*.domain/*` subdomain wildcards (and explicit `"https://*"` opt-in) in `DefaultOAuthRedirectPatterns()` and `oauth.allowed_redirect_uris`, covering all 6 Google Gemini Connected Apps callback environments (`*.googleusercontent.com`, `*.google.com`), `*.anthropic.com` / `claude.ai`, `*.openai.com` / `chatgpt.com`, and loopback out of the box while still rejecting arbitrary unapproved third-party callback domains by default.
- Default `oauth.access_token_ttl_seconds` to `2592000` (30 days, configurable up to 365 days) so Gemini Web sessions do not stall if the host skips background hourly token refresh.
- Emit structured zero-secret `amg_oauth` diagnostic events using server-owned route/method/status constants only.

### Filesystem Confinement & Out-of-the-Box Ergonomics

- Open `SKILL.md` and Skill support resources via `os.OpenRoot(skillBase)` with `O_RDONLY|syscall.O_NONBLOCK` and `IsRegular()` checks in `skills_models.go`, eliminating the `EvalSymlinks` -> `os.Open` TOCTOU window and blocking on non-regular entries such as FIFOs.
- Degrade individual absolute or dangling symlinks in `list_directory` to `symlink	-	<name>` via `os.Root.Lstat` instead of aborting the entire directory listing when `os.Root.Stat` enforces `RESOLVE_BENEATH` (for example on NixOS `~/.nix-profile`).
- Point newly connected agents in `initialize.instructions` directly to `agent-mcp-go-operations` and canonical `docs/configuration.md` for AMG server self-configuration.

See [v1.1.4 release notes](docs/releases/v1.1.4.md).

## 1.1.3

### Multi-agent operation

- Serialize task state per scope instead of globally. The notes and plans of one scope are written under a lock keyed by the resolved scope directory, so independent scopes run in parallel and one scope is never written twice at once. Review baselines serialize per repository and keep per-scope Git refs. A canceled caller releases its lock and never blocks another agent.
- `remember` keeps the existing top-level MCP tool surface: passing an empty `value` deletes that task-local note key of the current scope. This keeps the tool count identical to 1.1.2 and never touches shared long-term memory.
- Add an optional `workdir` argument to `run_command`. It is resolved against the configured `default_workdir` and must stay inside `work_root`; omitting it keeps the previous default. A missing path or a regular file is refused.
- Add a `default_workdir` configuration field. It selects the starting directory for relative paths and for tools that take no explicit directory, and must be an existing directory inside `work_root`.
- Remove task state of anonymous MCP sessions after 30 days without activity. Read-only `recall` and other existing-state reads refresh the session marker, so a read-heavy session keeps its TTL. Explicit scopes and ChatGPT conversation scopes are durable and are never removed by that sweep. Expired scopes are collected at startup and in a sweep that runs at most once an hour.

### Remote clients

- Add an `allowed_origins` configuration field for browser and desktop clients from another origin. Each entry is a bare origin with scheme and host, without path, query, fragment, or trailing slash, or `*` to accept every origin. Startup rejects malformed entries. The default stays unchanged: only the `public_url` origin is accepted.
- Keep Bearer authentication ahead of the Host and Origin checks, so a request without a valid token is still rejected as unauthorized regardless of its origin.

### OAuth clients

- Add an optional OAuth 2.1 authorization server for hosted MCP clients such as Google Gemini Web Connected Apps, Claude Web, and ChatGPT. It is off unless the new `oauth` configuration block is enabled, and it does not change the static owner Bearer token.
- Publish protected resource metadata (RFC 9728), authorization server metadata (RFC 8414), OIDC discovery, token, registration, and revocation endpoints. Gemini probes both the OAuth and OIDC metadata documents before it starts a flow.
- Support dynamic client registration (RFC 7591) for zero-credential setup, and pre-registered clients with a `client_id`, display name, redirect URIs, and an optional `client_secret_sha256` for the host's client-ID/client-secret fields.
- Issue authorization codes only after an owner-consent page accepts the Bearer token, and require PKCE `S256`, exact redirect matching, and CSRF-bound single-use consent tickets. Unknown or malformed `redirect_uri` values return 400 and never redirect.
- Refresh tokens do not rotate: a refresh returns a new access token with the same refresh token, and the OAuth identity stays stable per client so a stateful `Mcp-Session-Id` survives hourly refreshes without allowing cross-client session reuse.
- Accept OAuth access tokens on `/mcp` and `/mcp/stateless` through the official SDK's `auth.RequireBearerToken`. Unauthenticated requests now answer 401 with a `WWW-Authenticate` challenge that carries `resource_metadata` and `scope="mcp"`.
- Persist only SHA-256 digests of access tokens, refresh tokens, and client secrets, with mode `0600`. Dynamic clients, access tokens, and refresh tokens are capped, expired entries are swept on each request, and authorization codes and consent tickets are bounded in memory.
- Rate limit dynamic registration and passkey verification with `golang.org/x/time/rate`.

### Documentation

- Raise the bootstrap instructions budget from 24 KiB to 32 KiB and keep the Skills catalog preview at 13 KiB. The agent brief now names the default working directory.
- Document the multi-agent pattern: one explicit scope per agent, one `git worktree` per editing agent, and single-writer shared services.
- Document the OAuth configuration fields, the Gemini Web setup for both zero-credential and pre-registered clients, and the OAuth threat model.

See [v1.1.3 release notes](docs/releases/v1.1.3.md).

## 1.1.2

- Preserve existing task notes when reading their file fails; only a missing file starts an empty record. Reject JSON `null` instead of panicking on update.
- Check cancellation while scanning a text file, including lines skipped for a high offset.
- Make session input writes and output waits cancellable without replaying input or stopping the running session. A failed input write reports how many bytes were written; do not retry an entire partially written input.
- Distinguish shared-helper and upstream connection, call and close phases using the existing privacy-preserving diagnostics. Parallel fallback workers retain one parent scan stage.
- Keep module-first discovery, native MCP results, transfer contracts and ingress unchanged. These bounded fixes do not establish the root cause of earlier intermittent client timeouts.

## 1.1.1

- Add one in-flight slow warning after two seconds for active recognized RPCs, including the current server-owned stage and the existing request identifier. File open/read and command start/output-wait phases aid correlation without recording arguments or content.
- Keep HTTP/notification idle streams outside slow warnings, stop warnings at completion, and preserve cancellation, streaming and tool outcomes. No command retry or reconnect process is added.
- Exercise fresh-session initialize/list, task-scope continuity and lost-response writes with local official-SDK regression fixtures. Actual intermittent client timeouts remain unproven and require client-side correlation.
- Document bounded parallel read-only inspection and state inspection after a lost write response. Module-first navigation and owner boundaries remain unchanged.

## 1.1.0

### Module-first integration

- Initialization now directs agents to discover modules, list only a selected module, retrieve an exact schema on demand, and invoke a known tool without a listing preflight. `mcp_search_tools` accepts an empty query when `source` is provided.
- Module discovery and tool/schema listing reuse the canonical shared helper. Unknown tool counts are omitted instead of reported as zero; module purposes are not fabricated.
- AMG's global schema cache, background refresh, custom BM25 ranking, bilingual business dictionary and duplicate SDK catalog enumeration are removed. Unscoped search is an explicit bounded literal fallback with current-request failure reporting.
- Existing `shared_mcp_*` tools remain compatibility surfaces. Native tool invocation keeps the small official SDK adapter to preserve images, resources, structured results, `isError` and result metadata that a text helper envelope cannot carry directly.

### Compatibility and verification

- `force_refresh` remains accepted; listings are request-local and already fresh. The `score` field remains for wire compatibility, with no semantic-ranking claim. No retained index means no stale catalog rows are served.
- Workspace execution, transfer, attachments, review, task continuity and request diagnostics are unchanged. Client workflows still require user acceptance; ranking questionnaires do not establish it.

See [v1.1.0 release notes](docs/releases/v1.1.0.md).

## 1.0.3

### Improved

- Tool search preserves name, title, description and schema field boundaries. Per-field saturation limits repeated descriptive keywords; schema references remain available for recall without acting as strong execution intent.
- Authenticated MCP requests emit privacy-preserving request, review-stage, cancellation and completion diagnostics. Events contain server-owned labels and timing, with no commands, arguments, paths, credentials, results or raw errors. Commands are never retried automatically.

### Verification boundaries

- Ranking changes are regression-tested with fixed execution, capability-report and parameter-recall cases. These checks do not establish a new real-client Top3 percentage or resolve every reported network delay.
- File-transfer contracts and native attachment/review interfaces are unchanged. Client experience remains a separate manual acceptance surface.

See [v1.0.3 release notes](docs/releases/v1.0.3.md).

## 1.0.2

### Fixed

- Catalog enumeration now allows 30 seconds per route and a 45-second scan phase, keeping eight concurrent scans. The previous ten-second route cutoff could exclude a healthy service during initialization.
- Full paginated schemas are fetched through the official SDK into the existing in-process index. Exact lookup no longer accepts an abbreviated catalog entry without `inputSchema` or a different tool's schema.
- Stale catalog rows are labelled and excluded from exact-schema reuse. Partial failures retain safe per-source stage/category/duration diagnostics instead of only unavailable IDs.

### Limits and compatibility

- Catalog reads enforce per-source page, tool and metadata limits, HTTP/SSE size limits, cancellation, and cursor-cycle detection. No new persistent registry or shared-stack owner is introduced.
- Chinese lexical ranking, workspace and file-transfer interfaces, native attachment parameters, and review UI are unchanged. Real-client behavior and latency remain separate manual acceptance checks.

See [v1.0.2 release notes](docs/releases/v1.0.2.md).

## 1.0.1

### Fixed

- Permission-preservation tests now set the initial file mode explicitly, so restrictive `umask 077` does not turn a fixture assumption into a false runtime failure. CI uses that restrictive default.
- `import_host_file` retains its portable file-object/staged-file compatibility input without advertising that union as a host-native file field. The dedicated `import_chatgpt_file` file-object input remains the native attachment entry point.

### Improved

- Chinese and mixed-script tool search separates Han/Latin boundaries, avoids single-character noise, expands general business vocabulary, and lowers generic search-verb weights.
- Source-scoped search and exact schema lookup can reuse a fresh, successfully enumerated source from the existing in-process catalog.
- Global catalog scans have an overall budget and report incomplete results and unavailable sources explicitly, including work that could not start before the deadline.

### Client verification

- Server catalog and schema checks do not establish that a host has refreshed its inventory or granted attachment access. Real attachment import and review-card presentation still require client-specific verification.

See [v1.0.1 release notes](docs/releases/v1.0.1.md).

## 1.0.0

First public release of **AMG / Agent MCP Go**, a general-purpose remote agent workspace server over MCP.

### Added

- Official Go MCP SDK transport with authenticated stateful `/mcp` and stateless `/mcp/stateless` HTTP endpoints.
- Workspace file inspection, search, edits, structured patches, images, Shell process sessions, and Git operations.
- Streaming and chunked file transfer, no-clobber uploads by default, and MCP resource export.
- Change review resources, task plans and notes, scoped handover, and fresh initialization/discovery instructions.
- Optional on-demand consumption of existing local Skills and an external shared MCP CLI, including dynamic route search and native upstream MCP calls.
- Linux amd64 prebuilt archive and checksums, portable example configuration, and operator documentation.
- Explicit native Git status output for clean repositories, changes, and errors.
- Regular-file validation before text reads, rejecting FIFOs and devices without waiting for input.

### Support and limits

- Linux amd64 is the validated platform; source builds require Go 1.25+.
- Shell runs as the OS user without a sandbox. Task scopes are logical separation, not authorization.
- TTY programs are unsupported. Rollback is best effort and does not provide cross-process compare-and-swap.
- Host-native review cards and attachment behavior require client-specific verification.
- TLS ingress, shared services, and semantic memory are supplied separately by the operator.

See [v1.0.0 release notes](docs/releases/v1.0.0.md) for setup, verification commands, and known limitations.
