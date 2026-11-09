package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ExportInput struct {
	Path string `json:"path"`
}
type ExportOutput struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	Sha256      string `json:"sha256"`
	MimeType    string `json:"mimeType"`
	ExpiresInMs int64  `json:"expiresInMs"`
	URL         string `json:"url"`
}
type ImportInput struct {
	File  string `json:"file" jsonschema:"Legacy staged:<name> reference: usable only after an operator places the file in state_dir/staged. Generic clients upload directly to a work_root target with transfer_prepare."`
	Path  string `json:"path"`
	Scope string `json:"scope,omitempty"`
}
type ViewImageInput struct {
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty" jsonschema:"Optional image detail hint (auto, low, high); accepted for CodexFree compatibility. The original image is always returned."`
}
type ImportOutput struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
	Source string `json:"source"`
}

func (a *App) exportFile(ctx context.Context, in ExportInput) (ExportOutput, error) {
	c := a.config.Get()
	p, e := resolveWorkPath(c.WorkRoot, in.Path)
	if e != nil {
		return ExportOutput{}, e
	}
	source, e := openWorkspaceFile(c, p)
	if e != nil {
		return ExportOutput{}, e
	}
	defer source.Close()
	fi, e := source.Stat()
	if e != nil || !fi.Mode().IsRegular() || fi.Size() > c.MaxFileBytes {
		return ExportOutput{}, errors.New("export source is not a regular in-limit file")
	}
	state, e := os.OpenRoot(c.StateDir)
	if e != nil {
		return ExportOutput{}, e
	}
	defer state.Close()
	if e = state.MkdirAll("exports", 0700); e != nil {
		return ExportOutput{}, e
	}
	snapshots, e := state.OpenRoot("exports")
	if e != nil {
		return ExportOutput{}, e
	}
	retain := false
	defer func() {
		if !retain {
			_ = snapshots.Close()
		}
	}()
	dir, e := snapshots.Open(".")
	if e != nil {
		return ExportOutput{}, e
	}
	entries, e := dir.ReadDir(-1)
	_ = dir.Close()
	if e != nil {
		return ExportOutput{}, e
	}
	total := int64(0)
	for _, ent := range entries {
		f, e := snapshots.Lstat(ent.Name())
		if e != nil {
			continue
		}
		if time.Since(f.ModTime()) > 30*time.Minute {
			_ = snapshots.Remove(ent.Name())
			continue
		}
		total += f.Size()
	}
	if total+fi.Size() > 1024*1024*1024 {
		return ExportOutput{}, errors.New("snapshot cache 1 GiB limit exceeded")
	}
	tmp, name, e := createTransferStage(snapshots, "snapshot-")
	if e != nil {
		return ExportOutput{}, e
	}
	defer tmp.Close()
	defer func() {
		if !retain {
			_ = snapshots.Remove(name)
		}
	}()
	hasher := sha256.New()
	n, e := io.CopyN(io.MultiWriter(tmp, hasher), source, c.MaxFileBytes+1)
	if e != nil && e != io.EOF {
		return ExportOutput{}, e
	}
	if n > c.MaxFileBytes {
		return ExportOutput{}, errors.New("source changed beyond configured size")
	}
	if e = ctx.Err(); e != nil {
		return ExportOutput{}, e
	}
	if e = tmp.Sync(); e != nil {
		return ExportOutput{}, e
	}
	if e = tmp.Close(); e != nil {
		return ExportOutput{}, e
	}
	snapshotPath := filepath.Join(c.StateDir, "exports", name)
	item := ticket{direction: "download", path: snapshotPath, tempPath: snapshotPath, maxBytes: c.MaxFileBytes, name: filepath.Base(p), resource: &transferResource{parent: snapshots, target: name, removeTarget: true}}
	item.mimeType, _, _ = strings.Cut(mime.TypeByExtension(filepath.Ext(p)), ";")
	if item.mimeType == "" {
		item.mimeType = "application/octet-stream"
	}
	id, e := a.transfers.addTicket(item)
	if e != nil {
		return ExportOutput{}, e
	}
	retain = true
	base := strings.TrimRight(c.PublicURL, "/")
	if base == "" {
		base = "http://" + c.Listen
	}
	return ExportOutput{Name: item.name, Path: p, Bytes: n, Sha256: hex.EncodeToString(hasher.Sum(nil)), MimeType: item.mimeType, ExpiresInMs: 20 * 60 * 1000, URL: base + "/transfer/" + id}, nil
}
func (a *App) importFile(ctx context.Context, in ImportInput) (ImportOutput, error) {
	if !strings.HasPrefix(in.File, "staged:") {
		return ImportOutput{}, errors.New("expected a legacy staged:<name> reference already placed by an operator in state_dir/staged; generic clients upload directly to a work_root target with transfer_prepare")
	}
	id := strings.TrimPrefix(in.File, "staged:")
	if id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return ImportOutput{}, errors.New("invalid staged file reference")
	}
	c := a.config.Get()
	state, e := os.OpenRoot(c.StateDir)
	if e != nil {
		return ImportOutput{}, e
	}
	defer state.Close()
	staged, e := state.OpenRoot("staged")
	if e != nil {
		return ImportOutput{}, e
	}
	defer staged.Close()
	fi, e := staged.Lstat(id)
	if e != nil || !fi.Mode().IsRegular() || fi.Size() > c.MaxFileBytes {
		return ImportOutput{}, errors.New("staged file absent or oversized")
	}
	source, e := staged.OpenFile(id, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return ImportOutput{}, e
	}
	defer source.Close()
	fi, e = source.Stat()
	if e != nil || !fi.Mode().IsRegular() || fi.Size() > c.MaxFileBytes {
		return ImportOutput{}, errors.New("staged file absent or oversized")
	}
	dest, e := resolveWorkPath(c.WorkRoot, in.Path)
	if e != nil {
		return ImportOutput{}, e
	}
	parent, name, e := openTransferParent(c, dest)
	if e != nil {
		return ImportOutput{}, e
	}
	defer parent.Close()
	if _, e = parent.Lstat(name); !os.IsNotExist(e) {
		return ImportOutput{}, errors.New("destination exists or is inaccessible")
	}
	tmp, tmpName, e := createTransferStage(parent, ".agent-mcp-import-")
	if e != nil {
		return ImportOutput{}, e
	}
	defer tmp.Close()
	defer parent.Remove(tmpName)
	hash := sha256.New()
	n, e := io.CopyN(io.MultiWriter(tmp, hash), source, c.MaxFileBytes+1)
	if e != nil && e != io.EOF {
		return ImportOutput{}, e
	}
	if n > c.MaxFileBytes {
		return ImportOutput{}, errors.New("staged file changed beyond configured size")
	}
	if e = ctx.Err(); e != nil {
		return ImportOutput{}, e
	}
	if e = tmp.Sync(); e != nil {
		return ImportOutput{}, e
	}
	if e = tmp.Close(); e != nil {
		return ImportOutput{}, e
	}
	if e = commitTransferFile(parent, tmpName, name, false); e != nil {
		return ImportOutput{}, fmt.Errorf("destination exists or import failed: %w", e)
	}
	// Delete only after successful publication; rejected imports remain retryable.
	_ = staged.Remove(id)
	return ImportOutput{Path: dest, Bytes: n, Sha256: hex.EncodeToString(hash.Sum(nil)), Source: "staged_transfer"}, nil
}

type HostImportInput struct {
	File  json.RawMessage `json:"file"`
	Path  string          `json:"path"`
	Scope string          `json:"scope,omitempty"`
}

func (a *App) importHostFile(ctx context.Context, in HostImportInput, client *http.Client) (ChatGPTImportOutput, error) {
	if strings.TrimSpace(in.Path) == "" {
		return ChatGPTImportOutput{}, errors.New("attachment destination must be a new workspace file")
	}
	var staged string
	if json.Unmarshal(in.File, &staged) == nil {
		out, err := a.importFile(ctx, ImportInput{File: staged, Path: in.Path, Scope: in.Scope})
		return ChatGPTImportOutput{ImportOutput: out}, err
	}
	var fields map[string]*string
	if err := json.Unmarshal(in.File, &fields); err != nil || fields == nil || fields["download_url"] == nil || fields["file_id"] == nil {
		return ChatGPTImportOutput{}, errors.New("file must be a native file object or staged:<name> reference")
	}
	for key, value := range fields {
		switch key {
		case "download_url", "file_id", "mime_type", "file_name":
		default:
			return ChatGPTImportOutput{}, errors.New("native file object contains an unknown field")
		}
		if value == nil || strings.TrimSpace(*value) == "" || strings.ContainsFunc(*value, unicode.IsControl) {
			return ChatGPTImportOutput{}, errors.New("invalid native file metadata")
		}
	}
	file := ChatGPTFile{DownloadURL: *fields["download_url"], FileID: *fields["file_id"]}
	if mimeType := fields["mime_type"]; mimeType != nil {
		file.MIMEType = *mimeType
	}
	if name := fields["file_name"]; name != nil {
		file.FileName = *name
	}
	return a.importChatGPTFile(ctx, ChatGPTImportInput{File: file, Path: in.Path, Scope: in.Scope}, client)
}

func (a *App) registerFileParity(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "export_host_file", Title: "Download VPS file",
		Description: "Export an existing VPS file as an immutable download with its SHA-256. Files up to 32 MiB also return an MCP file resource for compatible hosts.",
		Meta:        mcp.Meta{"openai/toolInvocation/invoking": "Preparing file download", "openai/toolInvocation/invoked": "File ready to download"},
	},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ExportInput) (*mcp.CallToolResult, ExportOutput, error) {
			v, e := a.exportFile(ctx, in)
			if e != nil {
				return nil, v, e
			}
			if v.Bytes <= maxEmbeddedResourceBytes {
				uri := "agentmcp://artifact/" + filepath.Base(v.URL)
				size := v.Bytes
				return &mcp.CallToolResult{Content: []mcp.Content{
					&mcp.TextContent{Text: fmt.Sprintf("Exported %s (%d bytes, SHA-256 %s).", v.Name, v.Bytes, v.Sha256)},
					&mcp.ResourceLink{URI: uri, Name: v.Name, MIMEType: v.MimeType, Size: &size},
				}}, v, nil
			}
			return nil, v, nil
		})
	mcp.AddTool(server, &mcp.Tool{
		Name: "import_host_file", Title: "Import attached or staged file",
		Description: "Import an authorized native file object or a legacy staged:<name> reference into a new workspace path without overwrite. For ChatGPT attachment selection, use import_chatgpt_file. Legacy references require an operator to place the file in state_dir/staged. Generic clients use transfer_prepare to upload directly to a work_root target.",
		InputSchema: map[string]any{
			"type": "object",
			"$defs": map[string]any{"OpenAIFile": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"download_url": map[string]any{"type": "string", "minLength": 1},
					"file_id":      map[string]any{"type": "string", "minLength": 1},
					"mime_type":    map[string]any{"type": "string", "minLength": 1},
					"file_name":    map[string]any{"type": "string", "minLength": 1},
				},
				"required": []string{"download_url", "file_id"}, "additionalProperties": false,
			}},
			"properties": map[string]any{
				"file": map[string]any{"oneOf": []any{
					map[string]any{"$ref": "#/$defs/OpenAIFile"},
					map[string]any{"type": "string", "pattern": "^staged:", "minLength": 8, "description": "Legacy staged:<name> reference, available only after an operator places the file in state_dir/staged; generic clients upload directly to a work_root target with transfer_prepare."},
				}},
				"path":  map[string]any{"type": "string", "minLength": 1},
				"scope": map[string]any{"type": "string", "description": "Optional task identity for review continuity across reconnects."},
			},
			"required": []string{"file", "path"}, "additionalProperties": false,
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in HostImportInput) (*mcp.CallToolResult, ChatGPTImportOutput, error) {
		v, e := a.importHostFile(ctx, in, attachmentHTTP)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "view_image", Description: "Read a local PNG/JPEG/GIF/WebP/BMP file and return an MCP image content block, maximum 5 MiB."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ViewImageInput) (*mcp.CallToolResult, any, error) {
			c := a.config.Get()
			path, e := resolveWorkPath(c.WorkRoot, in.Path)
			if e != nil {
				return nil, nil, e
			}
			f, e := openWorkspaceFile(c, path)
			if e != nil {
				return nil, nil, e
			}
			defer f.Close()
			const imageCap = 5 * 1024 * 1024
			fi, e := f.Stat()
			if e != nil || !fi.Mode().IsRegular() || fi.Size() > imageCap {
				return nil, nil, errors.New("image is not a regular file below 5 MiB")
			}
			b, e := io.ReadAll(io.LimitReader(f, imageCap+1))
			if e != nil {
				return nil, nil, e
			}
			if len(b) > imageCap {
				return nil, nil, errors.New("image exceeds 5 MiB")
			}
			mime := http.DetectContentType(b)
			if strings.HasSuffix(strings.ToLower(path), ".webp") && len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
				mime = "image/webp"
			}
			if strings.HasSuffix(strings.ToLower(path), ".bmp") && len(b) > 2 && string(b[:2]) == "BM" {
				mime = "image/bmp"
			}
			valid := map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true}
			if !valid[mime] {
				return nil, nil, fmt.Errorf("unsupported image content type %q", mime)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{MIMEType: mime, Data: b}}}, nil, nil
		})
}
