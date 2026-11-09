package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type transferReadFailure struct{}

func (transferReadFailure) Read([]byte) (int, error) {
	return 0, errors.New("injected interrupted body")
}

func TestTransferBoundaryChunkFailureRetainsOffsetAndDescriptor(t *testing.T) {
	a, cs := boundaryApp(t)
	dest := filepath.Join(a.config.Get().WorkRoot, "file")
	id, ok := boundaryPrepare(t, cs, "upload", dest)
	if !ok {
		t.Fatal("prepare failed")
	}
	bad := io.MultiReader(strings.NewReader("bad"), transferReadFailure{})
	if w := boundaryRequest(a, id, "PATCH", 0, 6, bad); w.Code != 413 {
		t.Fatalf("interrupted chunk: %d %s", w.Code, w.Body.String())
	}
	item, ok := a.transfers.get(id)
	if !ok || item.offset != 0 || item.resource.file == nil {
		t.Fatal("failed chunk lost retry state")
	}
	fi, e := item.resource.file.Stat()
	if e != nil || fi.Size() != 0 {
		t.Fatalf("failed chunk did not roll back: %v %v", fi, e)
	}
	if w := boundaryRequest(a, id, "PATCH", 0, 6, strings.NewReader("abc")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stageFD := item.resource.file
	parent := item.resource.parent
	w := boundaryRequest(a, id, "PATCH", 3, 6, strings.NewReader("def"))
	sum := sha256.Sum256([]byte("abcdef"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), hex.EncodeToString(sum[:])) {
		t.Fatalf("final receipt: %d %s", w.Code, w.Body.String())
	}
	if _, e = stageFD.Stat(); !errors.Is(e, os.ErrClosed) {
		t.Errorf("completed stage descriptor not closed: %v", e)
	}
	if _, e = parent.Stat("."); e == nil {
		t.Error("completed parent root not closed")
	}
	b, e := os.ReadFile(dest)
	if e != nil || string(b) != "abcdef" {
		t.Fatalf("retry bytes: %q %v", b, e)
	}
}
func TestTransferBoundaryTicketLimitClosesRejectedAuthority(t *testing.T) {
	a, _ := boundaryApp(t)
	cfg := a.config.Get()
	for i := 0; i < 128; i++ {
		if _, e := a.transfers.prepareWorkspace(cfg, TransferInput{Direction: "upload", Path: filepath.Join(cfg.WorkRoot, "slot"+strconv.Itoa(i))}); e != nil {
			t.Fatal(e)
		}
	}
	parent, e := os.OpenRoot(cfg.WorkRoot)
	if e != nil {
		t.Fatal(e)
	}
	_, e = a.transfers.addTicket(ticket{direction: "upload", path: filepath.Join(cfg.WorkRoot, "rejected"), maxBytes: 100, resource: &transferResource{parent: parent, target: "rejected"}})
	if e == nil {
		t.Fatal("129th ticket accepted")
	}
	if _, e = parent.Stat("."); e == nil {
		t.Error("rejected ticket leaked authority root")
	}
	a.transfers.mu.Lock()
	defer a.transfers.mu.Unlock()
	if len(a.transfers.tickets) != 128 || len(a.transfers.uploadLocks) != 128 {
		t.Errorf("unbounded registry: %d/%d", len(a.transfers.tickets), len(a.transfers.uploadLocks))
	}
	for _, item := range a.transfers.tickets {
		ttl := time.Until(item.expiry)
		if ttl > 20*time.Minute || ttl < 19*time.Minute {
			t.Errorf("unexpected lease TTL %s", ttl)
		}
	}
}
func TestTransferBoundaryDownloadIndependentFDAndForget(t *testing.T) {
	a, cs := boundaryApp(t)
	dest := filepath.Join(a.config.Get().WorkRoot, "download")
	boundaryWrite(t, dest, "0123456789")
	id, ok := boundaryPrepare(t, cs, "download", dest)
	if !ok {
		t.Fatal("prepare failed")
	}
	first, item, e := a.transfers.openDownload(id)
	if e != nil {
		t.Fatal(e)
	}
	defer first.Close()
	second, _, e := a.transfers.openDownload(id)
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	parent := item.resource.parent
	if _, e = first.Seek(5, io.SeekStart); e != nil {
		t.Fatal(e)
	}
	one := make([]byte, 1)
	if _, e = second.Read(one); e != nil || string(one) != "0" {
		t.Fatalf("shared cursor: %q %v", one, e)
	}
	a.transfers.forget(id)
	if _, e = parent.Stat("."); e == nil {
		t.Error("forgotten download leaked root")
	}
	b, e := io.ReadAll(first)
	if e != nil || string(b) != "56789" {
		t.Fatalf("root cleanup closed independent FD: %q %v", b, e)
	}
	if _, _, e = a.transfers.openDownload(id); e == nil {
		t.Fatal("forgotten download reopened")
	}
}
func TestTransferBoundaryExpirySkipsInFlightChunk(t *testing.T) {
	a, cs := boundaryApp(t)
	dest := filepath.Join(a.config.Get().WorkRoot, "chunk")
	id, ok := boundaryPrepare(t, cs, "upload", dest)
	if !ok {
		t.Fatal("prepare failed")
	}
	if w := boundaryRequest(a, id, "PATCH", 0, 6, strings.NewReader("abc")); w.Code != 200 {
		t.Fatalf("first chunk: %d %s", w.Code, w.Body.String())
	}
	unlock, ok := a.transfers.lockUpload(id)
	if !ok {
		t.Fatal("missing ticket lock")
	}
	a.transfers.mu.Lock()
	item := a.transfers.tickets[id]
	item.expiry = time.Now().Add(-time.Second)
	a.transfers.tickets[id] = item
	a.transfers.mu.Unlock()
	stageFD, parent := item.resource.file, item.resource.parent
	a.transfers.sweepExpired()
	if _, e := stageFD.Stat(); e != nil {
		t.Errorf("sweep closed active stage: %v", e)
	}
	if _, e := parent.Stat("."); e != nil {
		t.Errorf("sweep closed active parent: %v", e)
	}
	unlock()
	a.transfers.sweepExpired()
	if _, e := stageFD.Stat(); !errors.Is(e, os.ErrClosed) {
		t.Errorf("expired stage not closed: %v", e)
	}
	if _, e := parent.Stat("."); e == nil {
		t.Error("expired parent not closed")
	}
	a.transfers.mu.Lock()
	defer a.transfers.mu.Unlock()
	if len(a.transfers.tickets) != 0 || len(a.transfers.uploadLocks) != 0 {
		t.Error("expired ticket retained")
	}
}
func TestTransferBoundaryPreparedParentCannotRedirectFirstRequest(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			a, cs := boundaryApp(t)
			parent := filepath.Join(a.config.Get().WorkRoot, "parent")
			if e := os.Mkdir(parent, 0700); e != nil {
				t.Fatal(e)
			}
			id, ok := boundaryPrepare(t, cs, "upload", filepath.Join(parent, "file"))
			if !ok {
				t.Fatal("prepare failed")
			}
			outside := t.TempDir()
			moved := boundarySwitch(t, parent, outside)
			w := boundaryRequest(a, id, method, 0, 3, strings.NewReader("new"))
			if _, e := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(e) {
				t.Errorf("prepared parent redirected outside: %v", e)
			}
			if w.Code == 200 {
				b, e := os.ReadFile(filepath.Join(moved, "file"))
				if e != nil || string(b) != "new" {
					t.Errorf("original authority failed: %q %v", b, e)
				}
			}
		})
	}
}
func TestTransferBoundaryImageCapAndInternalAlias(t *testing.T) {
	a, cs := boundaryApp(t)
	root := a.config.Get().WorkRoot
	// A recognized PNG signature is sufficient for the existing image-only contract.
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 5*1024*1024-8)...)
	path := filepath.Join(root, "large.png")
	if e := os.WriteFile(path, data, 0600); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(root, "alias.png")
	if e := os.Symlink("large.png", alias); e != nil {
		t.Fatal(e)
	}
	result, e := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": alias}})
	if e != nil || result.IsError {
		t.Fatalf("5MiB image or internal alias rejected: %v", e)
	}
	if len(result.Content) != 1 || result.StructuredContent != nil {
		t.Fatal("image-only content contract changed")
	}
	file, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if e != nil {
		t.Fatal(e)
	}
	_, e = file.Write([]byte{0})
	_ = file.Close()
	if e != nil {
		t.Fatal(e)
	}
	result, e = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": path}})
	if e != nil || !result.IsError {
		t.Fatalf("5MiB+1 image accepted: %v", e)
	}
}
func TestTransferBoundaryExportPrivateParentMovesAndCloses(t *testing.T) {
	a, cs := boundaryApp(t)
	cfg := a.config.Get()
	src := filepath.Join(cfg.WorkRoot, "source")
	boundaryWrite(t, src, "snapshot")
	out, e := a.exportFile(context.Background(), ExportInput{Path: src})
	if e != nil {
		t.Fatal(e)
	}
	id := filepath.Base(out.URL)
	item, ok := a.transfers.get(id)
	if !ok {
		t.Fatal("missing snapshot")
	}
	authority := item.resource.parent
	parent := filepath.Join(cfg.StateDir, "exports")
	outside := t.TempDir()
	moved := boundarySwitch(t, parent, outside)
	boundaryWrite(t, filepath.Join(outside, filepath.Base(item.path)), "OUTSIDE")
	w := boundaryRequest(a, id, "GET", 0, 0, nil)
	if w.Code != 200 || w.Body.String() != "snapshot" {
		t.Fatalf("snapshot redirected: %d %s", w.Code, w.Body.String())
	}
	resource, e := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "agentmcp://artifact/" + id})
	if e != nil || len(resource.Contents) != 1 || string(resource.Contents[0].Blob) != "snapshot" {
		t.Fatalf("MCP private snapshot redirected: %v %+v", e, resource)
	}
	a.transfers.forget(id)
	if _, e = authority.Stat("."); e == nil {
		t.Error("forgotten snapshot leaked root")
	}
	if _, e = os.Stat(filepath.Join(moved, filepath.Base(item.path))); !os.IsNotExist(e) {
		t.Errorf("forgotten private snapshot retained: %v", e)
	}
	b, e := os.ReadFile(filepath.Join(outside, filepath.Base(item.path)))
	if e != nil || string(b) != "OUTSIDE" {
		t.Errorf("snapshot cleanup escaped: %q %v", b, e)
	}
}

func TestTransferBoundaryInternalParentAlias(t *testing.T) {
	a, cs := boundaryApp(t)
	cfg := a.config.Get()
	parent := filepath.Join(cfg.WorkRoot, "parent")
	if e := os.Mkdir(parent, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(cfg.WorkRoot, "alias")
	if e := os.Symlink("parent", alias); e != nil {
		t.Fatal(e)
	}
	for _, method := range []string{"PUT", "PATCH"} {
		path := filepath.Join(alias, method)
		id, ok := boundaryPrepare(t, cs, "upload", path)
		if !ok {
			t.Fatal("internal alias upload rejected")
		}
		if w := boundaryRequest(a, id, method, 0, 3, strings.NewReader("new")); w.Code != 200 {
			t.Fatalf("internal upload %s: %d %s", method, w.Code, w.Body.String())
		}
		id, ok = boundaryPrepare(t, cs, "download", path)
		if !ok {
			t.Fatal("internal alias download rejected")
		}
		if w := boundaryRequest(a, id, "GET", 0, 0, nil); w.Code != 200 || w.Body.String() != "new" {
			t.Fatalf("internal download: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestTransferBoundaryLegacyImportAcrossFilesystems(t *testing.T) {
	if fi, e := os.Stat("/dev/shm"); e != nil || !fi.IsDir() {
		t.Skip("second native temporary filesystem unavailable")
	}
	root := t.TempDir()
	t.Run("private-stage", func(t *testing.T) {
		t.Setenv("TMPDIR", "/dev/shm")
		state := t.TempDir()
		stage := filepath.Join(state, "staged")
		if e := os.Mkdir(stage, 0700); e != nil {
			t.Fatal(e)
		}
		source := filepath.Join(stage, "one")
		boundaryWrite(t, source, "cross-filesystem")
		dest := filepath.Join(root, "imported")
		a := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, StateDir: state, MaxFileBytes: 1024}}}
		out, e := a.importFile(context.Background(), ImportInput{File: "staged:one", Path: dest})
		if e != nil || out.Bytes != 16 || out.Source != "staged_transfer" {
			t.Fatalf("cross-filesystem receipt: %+v %v", out, e)
		}
		b, e := os.ReadFile(dest)
		if e != nil || string(b) != "cross-filesystem" {
			t.Fatalf("cross-filesystem bytes: %q %v", b, e)
		}
		if _, e = os.Lstat(source); !os.IsNotExist(e) {
			t.Errorf("published stage retained: %v", e)
		}
	})
}

func TestTransferBoundaryOverwriteReplacesLeafLink(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			a, _ := boundaryApp(t)
			cfg := a.config.Get()
			outside := filepath.Join(t.TempDir(), "keep")
			boundaryWrite(t, outside, "original")
			dest := filepath.Join(cfg.WorkRoot, "leaf")
			if e := os.Symlink(outside, dest); e != nil {
				t.Fatal(e)
			}
			if _, e := a.transfers.prepareWorkspace(cfg, TransferInput{Direction: "upload", Path: dest}); e == nil {
				t.Fatal("default accepted existing leaf link")
			}
			id, e := a.transfers.prepareWorkspace(cfg, TransferInput{Direction: "upload", Path: dest, Overwrite: true})
			if e != nil {
				t.Fatal(e)
			}
			if w := boundaryRequest(a, id, method, 0, 3, strings.NewReader("new")); w.Code != 200 {
				t.Fatalf("explicit overwrite: %d %s", w.Code, w.Body.String())
			}
			b, e := os.ReadFile(outside)
			if e != nil || string(b) != "original" {
				t.Errorf("outside file changed: %q %v", b, e)
			}
			fi, e := os.Lstat(dest)
			if e != nil || !fi.Mode().IsRegular() {
				t.Errorf("overwrite did not replace link: %v %v", fi, e)
			}
			b, e = os.ReadFile(dest)
			if e != nil || string(b) != "new" {
				t.Errorf("overwrite bytes: %q %v", b, e)
			}
		})
	}
}

// Keep the HTTP Range path exercised with a root-bound independent descriptor.
func TestTransferBoundaryRangeAndHEAD(t *testing.T) {
	a, cs := boundaryApp(t)
	path := filepath.Join(a.config.Get().WorkRoot, "range")
	boundaryWrite(t, path, "0123456789")
	id, ok := boundaryPrepare(t, cs, "download", path)
	if !ok {
		t.Fatal("prepare failed")
	}
	req := httptest.NewRequest("GET", "/transfer/"+id, nil)
	req.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	a.transfers.handler(a).ServeHTTP(w, req)
	if w.Code != 206 || w.Body.String() != "2345" {
		t.Fatalf("Range changed: %d %s", w.Code, w.Body.String())
	}
	w = boundaryRequest(a, id, "HEAD", 0, 0, nil)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD changed: %d %s", w.Code, w.Body.String())
	}
}
