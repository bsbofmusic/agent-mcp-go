package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ChatGPTFile struct {
	DownloadURL string `json:"download_url"`
	FileID      string `json:"file_id"`
	MIMEType    string `json:"mime_type,omitempty"`
	FileName    string `json:"file_name,omitempty"`
}

type ChatGPTImportInput struct {
	File  ChatGPTFile `json:"file" jsonschema:"The file attached to this ChatGPT conversation, supplied by ChatGPT."`
	Path  string      `json:"path" jsonschema:"New destination within the workspace. Existing files are preserved."`
	Scope string      `json:"scope,omitempty"`
}

type ChatGPTImportOutput struct {
	ImportOutput
	MimeType string `json:"mimeType,omitempty"`
}

var errFileDownloadURL = errors.New("attachment must use a public HTTPS download URL")

var reservedDownloadNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicDownloadIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, network := range reservedDownloadNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

func validateAttachmentURL(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Hostname() == "" ||
		(u.Port() != "" && u.Port() != "443") {
		return errFileDownloadURL
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip, err := netip.ParseAddr(host); err == nil && !publicDownloadIP(ip) {
		return errFileDownloadURL
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errFileDownloadURL
	}
	return nil
}

var attachmentHTTP = &http.Client{
	Timeout: 2 * time.Minute,
	Transport: &http.Transport{
		// Check the resolved address on each dial. Never use ambient proxies.
		DialContext: (&net.Dialer{Timeout: 15 * time.Second, ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errFileDownloadURL
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !publicDownloadIP(ip) {
				return errFileDownloadURL
			}
			return nil
		}}).DialContext,
		TLSHandshakeTimeout:    15 * time.Second,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many attachment redirects")
		}
		req.Header.Del("Referer")
		return validateAttachmentURL(req.URL)
	},
}

func (a *App) importChatGPTFile(ctx context.Context, in ChatGPTImportInput, client *http.Client) (ChatGPTImportOutput, error) {
	file := in.File
	if strings.TrimSpace(file.FileID) == "" || len(file.FileID) > 512 || strings.ContainsFunc(file.FileID, unicode.IsControl) ||
		len(file.FileName) > 1024 || len(file.MIMEType) > 255 || strings.ContainsFunc(file.MIMEType, unicode.IsControl) {
		return ChatGPTImportOutput{}, errors.New("invalid ChatGPT attachment metadata")
	}
	if len(file.DownloadURL) > 16384 {
		return ChatGPTImportOutput{}, errFileDownloadURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.DownloadURL, nil)
	if err != nil || validateAttachmentURL(req.URL) != nil {
		return ChatGPTImportOutput{}, errFileDownloadURL
	}
	cfg := a.config.Get()
	target, err := resolveConfigPath(cfg, in.Path)
	if err != nil || strings.TrimSpace(in.Path) == "" ||
		target == filepath.Clean(cfg.WorkRoot) || target == cfg.defaultWorkdir() {
		return ChatGPTImportOutput{}, errors.New("attachment destination must be a new workspace file")
	}
	root, err := os.OpenRoot(cfg.WorkRoot)
	if err != nil {
		return ChatGPTImportOutput{}, err
	}
	defer root.Close()
	rel, err := filepath.Rel(cfg.WorkRoot, target)
	if err != nil {
		return ChatGPTImportOutput{}, err
	}
	if _, err := root.Lstat(rel); !errors.Is(err, os.ErrNotExist) {
		return ChatGPTImportOutput{}, errors.New("attachment destination exists or is inaccessible")
	}
	resp, err := client.Do(req)
	if err != nil {
		// http.Client errors may include the signed URL.
		if errors.Is(err, errFileDownloadURL) {
			return ChatGPTImportOutput{}, errFileDownloadURL
		}
		return ChatGPTImportOutput{}, errors.New("attachment download failed or expired")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ChatGPTImportOutput{}, fmt.Errorf("attachment download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > cfg.MaxFileBytes {
		return ChatGPTImportOutput{}, errors.New("attachment exceeds the file size limit")
	}
	dir, name := filepath.Split(rel)
	dir = filepath.Clean(dir)
	if err := root.MkdirAll(dir, 0700); err != nil {
		return ChatGPTImportOutput{}, err
	}
	parent, err := root.OpenRoot(dir)
	if err != nil {
		return ChatGPTImportOutput{}, err
	}
	defer parent.Close()
	id, err := randomID()
	if err != nil {
		return ChatGPTImportOutput{}, err
	}
	staged := ".amg-attachment-" + id
	f, err := parent.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ChatGPTImportOutput{}, err
	}
	defer parent.Remove(staged)
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, cfg.MaxFileBytes+1))
	if err != nil || n > cfg.MaxFileBytes || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return ChatGPTImportOutput{}, errors.New("attachment download incomplete or above the file size limit")
	}
	if err := ctx.Err(); err != nil {
		return ChatGPTImportOutput{}, err
	}
	if err := f.Sync(); err != nil {
		return ChatGPTImportOutput{}, err
	}
	if err := f.Close(); err != nil {
		return ChatGPTImportOutput{}, err
	}
	if err := parent.Link(staged, name); err != nil {
		return ChatGPTImportOutput{}, err
	}
	mimeType := file.MIMEType
	if mimeType == "" {
		mimeType = resp.Header.Get("Content-Type")
	}
	return ChatGPTImportOutput{ImportOutput: ImportOutput{
		Path: in.Path, Bytes: n, Sha256: "sha256:" + hex.EncodeToString(hash.Sum(nil)), Source: "openai_file",
	}, MimeType: mimeType}, nil
}

func (a *App) registerChatGPTImport(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "import_chatgpt_file", Title: "Save ChatGPT attachment to VPS",
		Description: "Use when the user attached a file in ChatGPT and asks to save it to the VPS. ChatGPT supplies the authorized file value. Pass the new destination path; this cannot overwrite a file.",
		Meta:        mcp.Meta{"openai/fileParams": []string{"file"}},
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatGPTImportInput) (*mcp.CallToolResult, ChatGPTImportOutput, error) {
		out, err := a.importChatGPTFile(ctx, in, attachmentHTTP)
		return nil, out, err
	})
}
