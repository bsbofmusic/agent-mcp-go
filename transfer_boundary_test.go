package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func boundaryApp(t *testing.T) (*App, *mcp.ClientSession) {
	t.Helper()
	a := &App{config: &ConfigStore{cfg: Config{WorkRoot: t.TempDir(), StateDir: t.TempDir(), MaxFileBytes: 8 << 20}}, transfers: NewTransferManager()}
	s := mcp.NewServer(&mcp.Implementation{Name: "transfer-boundary", Version: "1"}, nil)
	a.registerTransfer(s)
	a.registerFileParity(s)
	a.registerExportResources(s)
	cs := contractClient(t, s)
	t.Cleanup(func() {
		a.transfers.mu.Lock()
		ids := make([]string, 0, len(a.transfers.tickets))
		for id := range a.transfers.tickets {
			ids = append(ids, id)
		}
		a.transfers.mu.Unlock()
		for _, id := range ids {
			a.transfers.forget(id)
		}
	})
	return a, cs
}
func boundaryPrepare(t *testing.T, cs *mcp.ClientSession, direction, path string) (string, bool) {
	t.Helper()
	res, e := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "transfer_prepare", Arguments: map[string]any{"direction": direction, "path": path}})
	if e != nil {
		t.Fatal(e)
	}
	if res.IsError {
		return "", false
	}
	b, e := json.Marshal(res.StructuredContent)
	if e != nil {
		t.Fatal(e)
	}
	var out TransferOutput
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return filepath.Base(out.URL), true
}
func boundaryRequest(a *App, id, method string, off, total int64, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/transfer/"+id, body)
	if method == http.MethodPatch {
		req.Header.Set("Upload-Offset", strconv.FormatInt(off, 10))
		req.Header.Set("Upload-Length", strconv.FormatInt(total, 10))
	}
	w := httptest.NewRecorder()
	a.transfers.handler(a).ServeHTTP(w, req)
	return w
}
func boundaryWrite(t *testing.T, path, content string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
}
func boundarySwitch(t *testing.T, parent, outside string) string {
	t.Helper()
	moved := parent + "-moved"
	if e := os.Rename(parent, moved); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, parent); e != nil {
		t.Fatal(e)
	}
	return moved
}

type boundarySwitchReader struct {
	fn func()
	io.Reader
}

func (r *boundarySwitchReader) Read(p []byte) (int, error) {
	if r.fn != nil {
		fn := r.fn
		r.fn = nil
		fn()
	}
	return r.Reader.Read(p)
}

func TestTransferBoundaryDownloadAllowsSiblingRelativeSymlink(t *testing.T) {
	a, cs := boundaryApp(t)
	root := a.config.Get().WorkRoot
	for _, name := range []string{"a", "b"} {
		if e := os.Mkdir(filepath.Join(root, name), 0700); e != nil {
			t.Fatal(e)
		}
	}
	source := filepath.Join(root, "b", "file")
	boundaryWrite(t, source, "original")
	link := filepath.Join(root, "a", "link")
	if e := os.Symlink("../b/file", link); e != nil {
		t.Fatal(e)
	}
	id, ok := boundaryPrepare(t, cs, "download", link)
	if !ok {
		t.Fatal("workspace sibling relative symlink rejected")
	}
	if w := boundaryRequest(a, id, "GET", 0, 0, nil); w.Code != 200 || w.Body.String() != "original" {
		t.Fatalf("sibling symlink download: %d %q", w.Code, w.Body.String())
	}
	// Direct downloads permit in-place changes to the same file identity.
	boundaryWrite(t, source, "updated")
	if w := boundaryRequest(a, id, "GET", 0, 0, nil); w.Code != 200 || w.Body.String() != "updated" {
		t.Fatalf("same-identity content update: %d %q", w.Code, w.Body.String())
	}
	if e := os.Rename(source, source+"-original"); e != nil {
		t.Fatal(e)
	}
	boundaryWrite(t, source, "redirected")
	if w := boundaryRequest(a, id, "GET", 0, 0, nil); w.Code != 404 {
		t.Fatalf("replaced symlink target was served: %d %q", w.Code, w.Body.String())
	}
}

func TestTransferBoundaryDownloadRejectsLeafReplacement(t *testing.T) {
	a, cs := boundaryApp(t)
	path := filepath.Join(a.config.Get().WorkRoot, "file")
	boundaryWrite(t, path, "original")
	id, ok := boundaryPrepare(t, cs, "download", path)
	if !ok {
		t.Fatal("prepare failed")
	}
	if e := os.Rename(path, path+"-original"); e != nil {
		t.Fatal(e)
	}
	boundaryWrite(t, path, "redirected")
	if w := boundaryRequest(a, id, "GET", 0, 0, nil); w.Code != 404 {
		t.Fatalf("replaced direct file was served: %d %q", w.Code, w.Body.String())
	}
}

func TestTransferBoundaryDownloadRejectsParentRedirect(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(strconv.FormatBool(alias), func(t *testing.T) {
			a, cs := boundaryApp(t)
			cfg := a.config.Get()
			parent := filepath.Join(cfg.WorkRoot, "parent")
			original := parent
			if alias {
				original = filepath.Join(cfg.WorkRoot, "original")
			}
			other := filepath.Join(cfg.WorkRoot, "other")
			for _, dir := range []string{original, other} {
				if e := os.Mkdir(dir, 0700); e != nil {
					t.Fatal(e)
				}
			}
			boundaryWrite(t, filepath.Join(original, "file"), "original-parent")
			boundaryWrite(t, filepath.Join(other, "file"), "redirected-parent")
			if alias {
				if e := os.Symlink("original", parent); e != nil {
					t.Fatal(e)
				}
			}
			id, ok := boundaryPrepare(t, cs, "download", filepath.Join(parent, "file"))
			if !ok {
				t.Fatal("prepare failed")
			}
			if e := os.Rename(parent, parent+"-moved"); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink("other", parent); e != nil {
				t.Fatal(e)
			}
			w := boundaryRequest(a, id, "GET", 0, 0, nil)
			if w.Code != 404 && (w.Code != 200 || w.Body.String() != "original-parent") {
				t.Fatalf("download served redirected parent: %d %q", w.Code, w.Body.String())
			}
		})
	}
}

func TestTransferBoundaryExplicitOutsidePathsRejected(t *testing.T) {
	a, cs := boundaryApp(t)
	cfg := a.config.Get()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "file")
	boundaryWrite(t, outsideFile, "outside")
	rel, e := filepath.Rel(cfg.WorkRoot, outsideFile)
	if e != nil {
		t.Fatal(e)
	}
	if filepath.IsLocal(rel) {
		t.Fatal("fixture must be outside work_root")
	}
	for _, path := range []string{outsideFile, rel} {
		for _, direction := range []string{"upload", "download"} {
			if _, ok := boundaryPrepare(t, cs, direction, path); ok {
				t.Errorf("%s accepted outside path %q", direction, path)
			}
		}
		if _, e := a.exportFile(context.Background(), ExportInput{Path: path}); e == nil {
			t.Errorf("export accepted outside path %q", path)
		}
		res, e := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": path}})
		if e != nil || !res.IsError {
			t.Errorf("image accepted outside path %q: %v", path, e)
		}
	}
	stage := filepath.Join(cfg.StateDir, "staged")
	if e := os.Mkdir(stage, 0700); e != nil {
		t.Fatal(e)
	}
	boundaryWrite(t, filepath.Join(stage, "one"), "staged")
	for _, path := range []string{filepath.Join(outside, "new"), filepath.Join(filepath.Dir(rel), "new")} {
		if _, e := a.importFile(context.Background(), ImportInput{File: "staged:one", Path: path}); e == nil {
			t.Errorf("import accepted outside path %q", path)
		}
	}
	if b, e := os.ReadFile(filepath.Join(stage, "one")); e != nil || string(b) != "staged" {
		t.Errorf("rejected import consumed stage: %q %v", b, e)
	}
}

func TestTransferBoundaryLegacyStagedContract(t *testing.T) {
	_, cs := boundaryApp(t)
	tools, e := cs.ListTools(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name == "import_host_file" {
			found = true
			for _, term := range []string{"operator", "state_dir/staged", "work_root", "transfer_prepare"} {
				if !strings.Contains(tool.Description, term) {
					t.Errorf("import description omits %q: %s", term, tool.Description)
				}
			}
			schema := contractJSON(t, tool.InputSchema)
			properties := schema["properties"].(map[string]any)
			options := properties["file"].(map[string]any)["oneOf"].([]any)
			description := ""
			for _, option := range options {
				item := option.(map[string]any)
				if item["type"] == "string" {
					description, _ = item["description"].(string)
				}
			}
			for _, term := range []string{"operator", "state_dir/staged"} {
				if !strings.Contains(description, term) {
					t.Errorf("legacy schema omits %q: %s", term, description)
				}
			}
		}
	}
	if !found {
		t.Fatal("import_host_file missing")
	}
}

func TestTransferBoundaryLiveDirectoryUsesWorkRoot(t *testing.T) {
	for _, mode := range []string{"inside", "outside", "outside-alias"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			temp := outside
			expected := root
			if mode == "inside" {
				temp = filepath.Join(root, "tmp")
				expected = temp
				if e := os.Mkdir(temp, 0700); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "outside-alias" {
				temp = filepath.Join(root, "tmp")
				if e := os.Symlink(outside, temp); e != nil {
					t.Fatal(e)
				}
			}
			t.Setenv("TMPDIR", temp)
			cfg := Config{WorkRoot: root, StateDir: outside}
			dir := ""
			t.Run("fixture", func(t *testing.T) {
				dir = liveTransferDirectory(t, cfg)
				if filepath.Dir(dir) != expected {
					t.Fatalf("fixture chose %q, expected parent %q", dir, expected)
				}
				if _, e := workspaceRelativePath(cfg, dir); e != nil {
					t.Fatal(e)
				}
				boundaryWrite(t, filepath.Join(dir, "payload.bin"), "temporary")
			})
			if dir == "" {
				t.Fatal("fixture did not create directory")
			}
			if _, e := os.Lstat(dir); !os.IsNotExist(e) {
				t.Errorf("fixture directory retained: %v", e)
			}
		})
	}
}

func TestTransferBoundaryPrepareRejectsOutsideParents(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			a, cs := boundaryApp(t)
			outside := t.TempDir()
			boundaryWrite(t, filepath.Join(outside, "file"), "outside")
			alias := filepath.Join(a.config.Get().WorkRoot, "alias")
			if e := os.Symlink(outside, alias); e != nil {
				t.Fatal(e)
			}
			name := "file"
			if direction == "upload" {
				name = "new"
			}
			if _, ok := boundaryPrepare(t, cs, direction, filepath.Join(alias, name)); ok {
				t.Fatal("outside parent accepted")
			}
		})
	}
}
func TestTransferBoundaryParentSwitchDuringPUT(t *testing.T) {
	a, cs := boundaryApp(t)
	parent := filepath.Join(a.config.Get().WorkRoot, "parent")
	if e := os.Mkdir(parent, 0700); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	id, ok := boundaryPrepare(t, cs, "upload", filepath.Join(parent, "file"))
	if !ok {
		t.Fatal("prepare failed")
	}
	moved := ""
	body := &boundarySwitchReader{fn: func() {
		entries, e := os.ReadDir(parent)
		if e != nil || len(entries) != 1 {
			t.Fatalf("stage count %v %v", entries, e)
		}
		moved = boundarySwitch(t, parent, outside)
		boundaryWrite(t, filepath.Join(outside, entries[0].Name()), "outside-stage")
	}, Reader: strings.NewReader("payload")}
	w := boundaryRequest(a, id, "PUT", 0, 0, body)
	if _, e := os.Lstat(filepath.Join(outside, "file")); !os.IsNotExist(e) {
		t.Fatalf("PUT escaped: %v", e)
	}
	if w.Code == 200 {
		b, e := os.ReadFile(filepath.Join(moved, "file"))
		if e != nil || string(b) != "payload" {
			t.Fatalf("original authority lost: %q %v", b, e)
		}
	}
}
func TestTransferBoundaryChunkParentSwitch(t *testing.T) {
	a, cs := boundaryApp(t)
	parent := filepath.Join(a.config.Get().WorkRoot, "parent")
	if e := os.Mkdir(parent, 0700); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	id, ok := boundaryPrepare(t, cs, "upload", filepath.Join(parent, "file"))
	if !ok {
		t.Fatal("prepare failed")
	}
	if w := boundaryRequest(a, id, "PATCH", 0, 6, strings.NewReader("abc")); w.Code != 200 {
		t.Fatalf("first chunk: %d %s", w.Code, w.Body.String())
	}
	entries, e := os.ReadDir(parent)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 {
		t.Fatalf("stage count %d", len(entries))
	}
	stage := entries[0].Name()
	moved := boundarySwitch(t, parent, outside)
	boundaryWrite(t, filepath.Join(outside, stage), "OUTSIDE")
	w := boundaryRequest(a, id, "PATCH", 3, 6, strings.NewReader("def"))
	b, e := os.ReadFile(filepath.Join(outside, stage))
	if e != nil || string(b) != "OUTSIDE" {
		t.Fatalf("outside staging changed %q %v", b, e)
	}
	if _, e := os.Lstat(filepath.Join(outside, "file")); !os.IsNotExist(e) {
		t.Fatalf("chunk commit escaped: %v", e)
	}
	if w.Code == 200 {
		b, e = os.ReadFile(filepath.Join(moved, "file"))
		if e != nil || string(b) != "abcdef" {
			t.Fatalf("original commit %q %v", b, e)
		}
	}
}
func TestTransferBoundaryForgetAndExpiryCleanOriginalParent(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(strconv.FormatBool(expire), func(t *testing.T) {
			a, cs := boundaryApp(t)
			parent := filepath.Join(a.config.Get().WorkRoot, "parent")
			if e := os.Mkdir(parent, 0700); e != nil {
				t.Fatal(e)
			}
			id, ok := boundaryPrepare(t, cs, "upload", filepath.Join(parent, "file"))
			if !ok {
				t.Fatal("prepare failed")
			}
			if w := boundaryRequest(a, id, "PATCH", 0, 6, strings.NewReader("abc")); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			entries, e := os.ReadDir(parent)
			if e != nil || len(entries) != 1 {
				t.Fatalf("stage %v %v", entries, e)
			}
			stage := entries[0].Name()
			outside := t.TempDir()
			moved := boundarySwitch(t, parent, outside)
			boundaryWrite(t, filepath.Join(outside, stage), "keep")
			if expire {
				a.transfers.mu.Lock()
				item := a.transfers.tickets[id]
				item.expiry = time.Now().Add(-time.Second)
				a.transfers.tickets[id] = item
				a.transfers.mu.Unlock()
				a.transfers.sweepExpired()
			} else {
				a.transfers.forget(id)
			}
			if _, e := os.Lstat(filepath.Join(moved, stage)); !os.IsNotExist(e) {
				t.Errorf("original stage retained: %v", e)
			}
			b, e := os.ReadFile(filepath.Join(outside, stage))
			if e != nil || string(b) != "keep" {
				t.Errorf("outside cleanup changed file: %q %v", b, e)
			}
			if _, ok := a.transfers.get(id); ok {
				t.Error("ticket retained")
			}
		})
	}
}
func TestTransferBoundaryDownloadRejectsOutsideRedirect(t *testing.T) {
	a, cs := boundaryApp(t)
	root := a.config.Get().WorkRoot
	parent := filepath.Join(root, "selected")
	if e := os.Mkdir(parent, 0700); e != nil {
		t.Fatal(e)
	}
	boundaryWrite(t, filepath.Join(parent, "file"), "original")
	id, ok := boundaryPrepare(t, cs, "download", filepath.Join(parent, "file"))
	if !ok {
		t.Fatal("prepare failed")
	}
	outside := t.TempDir()
	boundaryWrite(t, filepath.Join(outside, "file"), "OUTSIDE")
	boundarySwitch(t, parent, outside)
	w := boundaryRequest(a, id, "GET", 0, 0, nil)
	if w.Code != 404 && (w.Code != 200 || w.Body.String() != "original") {
		t.Fatalf("download served outside redirect: %d %q", w.Code, w.Body.String())
	}
}
func TestTransferBoundaryExportPrivateStateOutsideWorkRoot(t *testing.T) {
	a, _ := boundaryApp(t)
	src := filepath.Join(a.config.Get().WorkRoot, "file.txt")
	boundaryWrite(t, src, "snapshot")
	out, e := a.exportFile(context.Background(), ExportInput{Path: src})
	if e != nil {
		t.Fatal(e)
	}
	w := boundaryRequest(a, filepath.Base(out.URL), "GET", 0, 0, nil)
	if w.Code != 200 || w.Body.String() != "snapshot" {
		t.Fatalf("private snapshot rejected: %d %q", w.Code, w.Body.String())
	}
}
func TestTransferBoundaryLegacyStagedRoots(t *testing.T) {
	for _, sourceEscape := range []bool{false, true} {
		t.Run(strconv.FormatBool(sourceEscape), func(t *testing.T) {
			a, _ := boundaryApp(t)
			cfg := a.config.Get()
			outside := t.TempDir()
			stage := filepath.Join(cfg.StateDir, "staged")
			dest := filepath.Join(cfg.WorkRoot, "dest")
			if sourceEscape {
				boundaryWrite(t, filepath.Join(outside, "one"), "private")
				if e := os.Symlink(outside, stage); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.Mkdir(stage, 0700); e != nil {
					t.Fatal(e)
				}
				boundaryWrite(t, filepath.Join(stage, "one"), "payload")
				alias := filepath.Join(cfg.WorkRoot, "alias")
				if e := os.Symlink(outside, alias); e != nil {
					t.Fatal(e)
				}
				dest = filepath.Join(alias, "dest")
			}
			if _, e := a.importFile(context.Background(), ImportInput{File: "staged:one", Path: dest}); e == nil {
				t.Fatal("escaped staged import accepted")
			}
			if _, e := os.Lstat(filepath.Join(outside, "dest")); !os.IsNotExist(e) {
				t.Errorf("outside destination created: %v", e)
			}
		})
	}
}
func TestTransferBoundaryLegacyImportCopiesPrivateSnapshot(t *testing.T) {
	a, _ := boundaryApp(t)
	cfg := a.config.Get()
	stage := filepath.Join(cfg.StateDir, "staged")
	if e := os.Mkdir(stage, 0700); e != nil {
		t.Fatal(e)
	}
	src := filepath.Join(stage, "one")
	witness := filepath.Join(stage, "witness")
	boundaryWrite(t, src, "payload")
	if e := os.Link(src, witness); e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(cfg.WorkRoot, "dest")
	out, e := a.importFile(context.Background(), ImportInput{File: "staged:one", Path: dest})
	if e != nil || out.Bytes != 7 || out.Source != "staged_transfer" {
		t.Fatalf("receipt %+v %v", out, e)
	}
	boundaryWrite(t, witness, "changed")
	b, e := os.ReadFile(dest)
	if e != nil || string(b) != "payload" {
		t.Fatalf("staged inode still shared: %q %v", b, e)
	}
}
func TestTransferBoundaryImageRejectsMiddleOutsideLink(t *testing.T) {
	a, cs := boundaryApp(t)
	outside := t.TempDir()
	var img bytes.Buffer
	if e := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 1, 1))); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(outside, "image.png"), img.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(a.config.Get().WorkRoot, "selected")
	if e := os.Symlink(outside, alias); e != nil {
		t.Fatal(e)
	}
	res, e := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": filepath.Join(alias, "image.png")}})
	if e != nil {
		t.Fatal(e)
	}
	if !res.IsError {
		t.Fatal("image outside root exposed")
	}
}
