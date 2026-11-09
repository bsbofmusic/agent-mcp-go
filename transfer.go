package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TransferInput struct {
	Direction string `json:"direction" jsonschema:"upload or download"`
	Scope     string `json:"scope,omitempty"`
	Path      string `json:"path" jsonschema:"Absolute VPS target path."`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"For upload only: explicitly allow replacing an existing file. False by default to protect data."`
}
type TransferOutput struct {
	Method         string `json:"method"`
	URL            string `json:"url"`
	MaxBytes       int64  `json:"max_bytes"`
	ExpiresMinutes int    `json:"expires_minutes"`
	Note           string `json:"note"`
}
type ticket struct {
	direction        string
	path             string
	overwrite        bool
	expiry           time.Time
	maxBytes         int64
	tempPath         string // Private export snapshot identity, also used by MCP resources.
	name             string
	mimeType         string
	offset           int64
	total            int64
	resource         *transferResource
	downloadIdentity os.FileInfo // Direct workspace downloads; nil for private owner snapshots.
}

// Uploads and private snapshots pin their containing directory. Direct downloads
// retain the workspace root and verify each opened file against the identity
// observed during preparation. In-place content changes are not a snapshot.
type transferResource struct {
	parent       *os.Root
	target       string
	staged       string
	file         *os.File
	removeTarget bool
}

func (r *transferResource) close() {
	if r == nil {
		return
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	if r.parent != nil {
		if r.staged != "" {
			_ = r.parent.Remove(r.staged)
			r.staged = ""
		}
		if r.removeTarget {
			_ = r.parent.Remove(r.target)
		}
		_ = r.parent.Close()
		r.parent = nil
	}
}

type TransferManager struct {
	mu          sync.Mutex
	tickets     map[string]ticket
	uploadLocks map[string]*sync.Mutex
}

const transferUploadReadTimeout = 90 * time.Second

func NewTransferManager() *TransferManager {
	return &TransferManager{tickets: map[string]ticket{}, uploadLocks: map[string]*sync.Mutex{}}
}

// Called with the map lock held; never wait for an active upload here.
func (t *TransferManager) expireLocked(id string, item ticket) bool {
	if !time.Now().After(item.expiry) {
		return false
	}
	if lock := t.uploadLocks[id]; lock != nil {
		if !lock.TryLock() {
			return true
		}
		defer lock.Unlock()
	}
	item.resource.close()
	delete(t.tickets, id)
	delete(t.uploadLocks, id)
	return true
}
func (t *TransferManager) lockUpload(id string) (func(), bool) {
	t.mu.Lock()
	lock := t.uploadLocks[id]
	t.mu.Unlock()
	if lock == nil {
		return nil, false
	}
	lock.Lock()
	return lock.Unlock, true
}
func (t *TransferManager) addTicket(item ticket) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, item := range t.tickets {
		t.expireLocked(id, item)
	}
	if len(t.tickets) >= 128 {
		item.resource.close()
		return "", errors.New("too many pending transfers")
	}
	id, err := randomID()
	if err != nil {
		item.resource.close()
		return "", err
	}
	if t.tickets == nil {
		t.tickets = map[string]ticket{}
	}
	if t.uploadLocks == nil {
		t.uploadLocks = map[string]*sync.Mutex{}
	}
	item.expiry = time.Now().Add(20 * time.Minute)
	t.tickets[id] = item
	t.uploadLocks[id] = &sync.Mutex{}
	return id, nil
}

// Internal callers that already own an absolute private path (and old test
// fixtures) grant authority over its parent. Client paths use prepareWorkspace.
func (t *TransferManager) create(direction, path string, maxBytes int64) (string, error) {
	return t.createWithOverwrite(direction, path, maxBytes, false)
}
func (t *TransferManager) createWithOverwrite(direction, path string, maxBytes int64, overwrite bool) (string, error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return t.addTicket(ticket{direction: direction, path: path, maxBytes: maxBytes, overwrite: overwrite, resource: &transferResource{parent: parent, target: filepath.Base(path)}})
}

// openTransferParent resolves and creates only beneath the workspace root,
// then pins the destination directory so later aliases cannot redirect IO.
func openTransferParent(c Config, path string) (*os.Root, string, error) {
	root, rel, err := openWorkspaceRoot(c, path)
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	if rel == "." {
		return nil, "", errors.New("destination must name a file")
	}
	dir, name := filepath.Split(rel)
	dir = filepath.Clean(dir)
	if err = root.MkdirAll(dir, 0700); err != nil {
		return nil, "", err
	}
	parent, err := root.OpenRoot(dir)
	return parent, name, err
}
func createTransferStage(parent *os.Root, prefix string) (*os.File, string, error) {
	id, err := randomID()
	if err != nil {
		return nil, "", err
	}
	name := prefix + id
	file, err := parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	return file, name, err
}
func (t *TransferManager) prepareWorkspace(c Config, in TransferInput) (string, error) {
	if in.Direction != "upload" && in.Direction != "download" {
		return "", errors.New("direction must be upload or download")
	}
	path, err := resolveWorkPath(c.WorkRoot, in.Path)
	if err != nil {
		return "", err
	}
	item := ticket{direction: in.Direction, path: path, maxBytes: c.MaxFileBytes, overwrite: in.Direction == "upload" && in.Overwrite}
	if in.Direction == "download" {
		root, rel, err := openWorkspaceRoot(c, path)
		if err != nil {
			return "", err
		}
		item.resource = &transferResource{parent: root, target: rel}
		f, err := openTransferDownload(item)
		if err != nil {
			item.resource.close()
			return "", err
		}
		item.downloadIdentity, err = f.Stat()
		_ = f.Close()
		if err != nil {
			item.resource.close()
			return "", err
		}
	} else {
		parent, name, err := openTransferParent(c, path)
		if err != nil {
			return "", err
		}
		item.resource = &transferResource{parent: parent, target: name}
		if !in.Overwrite {
			if _, err = parent.Lstat(name); err == nil {
				item.resource.close()
				return "", errors.New("upload destination already exists; use overwrite=true only when intentional")
			} else if !os.IsNotExist(err) {
				item.resource.close()
				return "", err
			}
		}
	}
	return t.addTicket(item)
}
func (t *TransferManager) get(id string) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	item, ok := t.tickets[id]
	if !ok || t.expireLocked(id, item) {
		return ticket{}, false
	}
	return item, true
}

// forgetLocked is called while holding this ticket's mutex, never a body read
// under the registry mutex. Public forget waits outside the registry lock.
func (t *TransferManager) forgetLocked(id string) {
	t.mu.Lock()
	item, ok := t.tickets[id]
	delete(t.tickets, id)
	delete(t.uploadLocks, id)
	t.mu.Unlock()
	if ok {
		item.resource.close()
	}
}
func (t *TransferManager) forget(id string) {
	unlock, ok := t.lockUpload(id)
	if !ok {
		return
	}
	defer unlock()
	t.forgetLocked(id)
}
func openTransferDownload(item ticket) (*os.File, error) {
	r := item.resource
	if r == nil || r.parent == nil {
		return nil, errors.New("download unavailable")
	}
	f, err := r.parent.OpenFile(r.target, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > item.maxBytes {
		_ = f.Close()
		return nil, errors.New("not a regular file or exceeds max_file_bytes")
	}
	if item.downloadIdentity != nil && !os.SameFile(item.downloadIdentity, fi) {
		_ = f.Close()
		return nil, errors.New("download file identity changed")
	}
	return f, nil
}

// The caller owns the returned descriptor. Root cleanup cannot close it and
// concurrent ranges have separate seek cursors. Only opening takes the lock.
func (t *TransferManager) openDownload(id string) (*os.File, ticket, error) {
	unlock, ok := t.lockUpload(id)
	if !ok {
		return nil, ticket{}, os.ErrNotExist
	}
	defer unlock()
	item, ok := t.get(id)
	if !ok || item.direction != "download" {
		return nil, ticket{}, os.ErrNotExist
	}
	f, err := openTransferDownload(item)
	return f, item, err
}
func (a *App) registerTransfer(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "transfer_prepare", Description: "Create a 20-minute single-purpose HTTP upload/download URL within work_root. Uploads never replace an existing target unless overwrite=true is explicitly set. Downloads bind to the prepared file identity and reject replacement; in-place content changes remain visible. Use export_host_file for an immutable snapshot. Stream in bounded chunks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in TransferInput) (*mcp.CallToolResult, TransferOutput, error) {
			c := a.config.Get()
			id, err := a.transfers.prepareWorkspace(c, in)
			if err != nil {
				return nil, TransferOutput{}, err
			}
			method := "PUT"
			if in.Direction == "download" {
				method = "GET"
			}
			base := strings.TrimRight(c.PublicURL, "/")
			if base == "" {
				base = "http://" + c.Listen
			}
			return nil, TransferOutput{Method: method, URL: base + "/transfer/" + id, MaxBytes: c.MaxFileBytes, ExpiresMinutes: 20, Note: "Treat ticket URL as a secret. For uploads over 16 MiB use sequential PATCH chunks with Upload-Offset, Upload-Length and Upload-Complete headers (each <=16 MiB); downloads support HTTP Range and bind to the prepared file identity, rejecting replacement while allowing in-place content changes. Use export_host_file for an immutable snapshot."}, nil
		})
}
func (t *TransferManager) handler(a *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/transfer/")
		if len(id) != 40 || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		item, ok := t.get(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if item.direction == "download" {
			if r.Method != "GET" && r.Method != "HEAD" {
				http.Error(w, "method not allowed", 405)
				return
			}
			file, item, err := t.openDownload(id)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer file.Close()
			fi, err := file.Stat()
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > item.maxBytes {
				http.Error(w, "file unavailable or above limit", 413)
				return
			}
			name := item.name
			if name == "" {
				name = filepath.Base(item.path)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			http.ServeContent(w, r, name, fi.ModTime(), file)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(transferUploadReadTimeout))
		if r.Method == "PATCH" {
			t.chunkedUpload(w, r, id)
			return
		}
		if r.Method != "PUT" {
			http.Error(w, "method not allowed", 405)
			return
		}
		unlock, ok := t.lockUpload(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		defer unlock()
		item, ok = t.get(id)
		if !ok || item.direction != "upload" || item.resource == nil {
			http.NotFound(w, r)
			return
		}
		resource := item.resource
		if resource.staged != "" {
			http.Error(w, "chunked upload already started", http.StatusConflict)
			return
		}
		if r.ContentLength > item.maxBytes {
			http.Error(w, "file above limit", 413)
			return
		}
		f, staged, err := createTransferStage(resource.parent, ".agent-mcp-upload-")
		if err != nil {
			http.Error(w, "upload staging unavailable", 500)
			return
		}
		defer resource.parent.Remove(staged)
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), http.MaxBytesReader(w, r.Body, item.maxBytes))
		if err != nil {
			http.Error(w, "upload exceeds limit or interrupted", 413)
			return
		}
		if err = f.Sync(); err != nil {
			http.Error(w, "disk sync failed", 500)
			return
		}
		if err = f.Close(); err != nil {
			http.Error(w, "disk close failed", 500)
			return
		}
		if err = commitTransferFile(resource.parent, staged, resource.target, item.overwrite); err != nil {
			if errors.Is(err, os.ErrExist) {
				http.Error(w, "destination already exists; upload did not overwrite it", 409)
			} else {
				http.Error(w, "save failed", 500)
			}
			return
		}
		// Remove before closing the authority root; the deferred Remove is harmless.
		_ = resource.parent.Remove(staged)
		t.forgetLocked(id)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "saved_bytes=%d sha256=%s\n", n, hex.EncodeToString(h.Sum(nil)))
	})
}
func commitTransferFile(parent *os.Root, staged, destination string, overwrite bool) error {
	if overwrite {
		return parent.Rename(staged, destination)
	}
	if err := parent.Link(staged, destination); err != nil {
		return err
	}
	_ = parent.Remove(staged)
	return nil
}
