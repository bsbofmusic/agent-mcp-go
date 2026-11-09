package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxEmbeddedResourceBytes = 32 * 1024 * 1024

// Exported resource identities are random capabilities, not enumerable files.
// Large payloads remain on the signed HTTP transfer path to bound MCP memory.
func (a *App) registerExportResources(server *mcp.Server) {
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "agentmcp://artifact/{id}",
		Name:        "Exported immutable file snapshot",
		Description: "Read an immutable exported file snapshot (up to 32 MiB); use its HTTPS transfer URL for larger files.",
	}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		raw := request.Params.URI
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme != "agentmcp" || parsed.Host != "artifact" {
			return nil, mcp.ResourceNotFoundError(raw)
		}
		id := strings.TrimPrefix(parsed.Path, "/")
		if len(id) != 40 || strings.Contains(id, "/") {
			return nil, mcp.ResourceNotFoundError(raw)
		}
		f, ticket, e := a.transfers.openDownload(id)
		if e != nil {
			return nil, mcp.ResourceNotFoundError(raw)
		}
		defer f.Close()
		if ticket.direction != "download" || ticket.tempPath == "" {
			return nil, mcp.ResourceNotFoundError(raw)
		}
		expectedDir := filepath.Join(a.config.Get().StateDir, "exports")
		if filepath.Dir(ticket.path) != expectedDir {
			return nil, mcp.ResourceNotFoundError(raw)
		}
		fi, e := f.Stat()
		if e != nil || !fi.Mode().IsRegular() || fi.Size() > maxEmbeddedResourceBytes {
			return nil, errors.New("resource too large for MCP; use its signed HTTPS URL")
		}
		b, e := io.ReadAll(io.LimitReader(f, maxEmbeddedResourceBytes+1))
		if e != nil {
			return nil, e
		}
		if len(b) > maxEmbeddedResourceBytes {
			return nil, errors.New("MCP resource size exceeded")
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: raw, MIMEType: ticket.mimeType, Blob: b,
		}}}, nil
	})
}
