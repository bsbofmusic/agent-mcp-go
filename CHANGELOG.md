# Changelog

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
