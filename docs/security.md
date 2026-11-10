# Security model

AMG grants authenticated clients the ability to read and modify workspace files and execute commands as the server's OS user. **Possession of its Bearer token is trusted OS-user code-execution authority.** Deploy it only for clients and operators you trust with that account.

## OS account and workspace boundary

Use a dedicated unprivileged account with only the files and permissions it needs. Keep the token, configuration, and private state outside `work_root`, and avoid granting the account unrestricted sudo or access to unrelated secrets.

Workspace file operations use root-relative filesystem access (`os.Root`) and path checks to prevent traversal and symlink escapes from the configured workspace. This protects the built-in file interface. Shell commands execute with the account's ordinary OS permissions and inherited environment; **Shell is not an OS sandbox** and can access locations outside `work_root` when those permissions allow it. Git and external programs also execute as that account. Use OS/container isolation if your threat model requires stronger boundaries.

Ignore patterns are discovery filters, not access controls. Project instructions, Skill documents, files, and upstream tool results are untrusted input to an agent; inspect them with the same care as other source material.

## Authentication and network exposure

AMG refuses a non-loopback listener. Expose it remotely only through your own HTTPS reverse proxy or tunnel, using the correct `public_url` and preserving required headers. HTTP Host and browser Origin checks supplement Bearer authentication; they do not replace it.

Create a random token with at least 32 characters after trimming whitespace and store it at an absolute path with mode `0600` in a private directory. Startup rejects group/other file permissions. Use restrictive permissions for configuration and state too. The server reads the token at startup; rotate by replacing the private token, restarting AMG, and updating authorized clients and ingress credentials as applicable.

Do not put tokens in repository files, URLs, screenshots, shell history, or shared logs. Do not expose your server process environment to untrusted users. MCP authentication uses a static Bearer header; no OAuth flow or per-tool user authorization is included.

`/healthz` is unauthenticated and returns `ok\n`. It does not inspect authorization or establish end-to-end MCP health.

## Transfer URLs are credentials

Authenticated tools can issue upload/download URLs with **160-bit random identifiers** and a **20-minute lifetime**. Requests to these capability URLs do not require the MCP Bearer header. Anyone with a live URL can exercise the authority granted by that URL until expiry; protect the entire URL as a secret.

Avoid logging `/transfer/` paths in public analytics or access logs, and use HTTPS remotely. Tokens/tickets are process-local and do not survive restart. Uploads default to no-clobber; explicit `overwrite=true` permits replacement. Limits apply at AMG and may also apply at the proxy or client.

## Scope and concurrency limits

Explicit scopes, ChatGPT metadata, and MCP session identities partition task records into distinct namespaces. This is logical organization for trusted clients sharing an OS principal, **not an ACL**. A caller with the Bearer token and Shell authority can inspect files available to the account, including state. Never use scopes to separate mutually untrusted users.

An empty task does not automatically import another scope's plan or notes. The same explicit scope can share saved plans, notes, and project context across clients; it does not share chat transcripts or restore active Shell sessions.

Review/patch rollback is best effort. Project locks coordinate operations within one server process; they do not provide cross-process compare-and-swap or a transaction against external editors, Git commands, or other AMG instances. Avoid concurrent writers to the same project and retain independent version-control/backups for valuable work.

## Request diagnostics

Authenticated MCP traffic emits server-generated request identifiers, parent correlation, allowlisted MCP method/tool names, phases, outcomes and elapsed milliseconds to the process logger. Review phases distinguish repository lookup, lock wait, baseline capture and handler dispatch. Cancellation is recorded even while a handler is still unwinding. The HTTP outcome `returned` means the transport handler returned; the corresponding RPC outcome describes tool success or failure.

These events omit commands, arguments, paths, scopes, client identifiers, headers, request bodies, results and raw exception text. Unknown method/tool labels become `other`. Transfer capability URLs are never included. Logging does not retry a command or establish the root cause of an earlier timeout. Retention and access control follow your existing process-log or journal policy; no separate log database is created.

## Reporting a vulnerability

Do not post tokens, private URLs, confidential files, or a working exploit against a live deployment in a public issue. Check the [repository's security advisories](https://github.com/bsbofmusic/agent-mcp-go/security/advisories) for an available private reporting option. If private reporting is unavailable, open a minimal issue requesting a private contact without disclosing exploit details. This project does not promise a response SLA or a managed security service.
