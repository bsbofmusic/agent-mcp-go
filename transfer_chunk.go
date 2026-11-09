package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
)

const chunkLimit = int64(16 * 1024 * 1024)

// Requests serialize only on their own ticket. The fixed parent and staging
// descriptor persist across chunks, so renamed directories cannot redirect IO.
func (t *TransferManager) chunkedUpload(w http.ResponseWriter, r *http.Request, id string) {
	off, e1 := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	total, e2 := strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	if e1 != nil || e2 != nil || off < 0 || total <= 0 {
		http.Error(w, "Upload-Offset and Upload-Length are required", 400)
		return
	}
	if r.ContentLength > chunkLimit {
		http.Error(w, "chunk exceeds 16 MiB", 413)
		return
	}
	unlock, ok := t.lockUpload(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer unlock()
	item, ok := t.get(id)
	if !ok || item.direction != "upload" || item.resource == nil {
		http.NotFound(w, r)
		return
	}
	if total > item.maxBytes {
		http.Error(w, "file exceeds configured size", 413)
		return
	}
	if off != item.offset {
		w.Header().Set("Upload-Offset", strconv.FormatInt(item.offset, 10))
		http.Error(w, "wrong chunk offset", 409)
		return
	}
	if item.total != 0 && total != item.total {
		http.Error(w, "upload length cannot change", 409)
		return
	}
	resource := item.resource
	if resource.staged == "" {
		f, name, err := createTransferStage(resource.parent, ".agent-mcp-chunk-")
		if err != nil {
			http.Error(w, "staging unavailable", 500)
			return
		}
		resource.file = f
		resource.staged = name
		item.total = total
		t.mu.Lock()
		t.tickets[id] = item
		t.mu.Unlock()
	}
	f := resource.file
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		http.Error(w, "staging unavailable", 500)
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, chunkLimit))
	if err != nil || n == 0 || n > total-off {
		_ = f.Truncate(off)
		http.Error(w, "chunk interrupted, oversized or empty", 413)
		return
	}
	if err = f.Sync(); err != nil {
		_ = f.Truncate(off)
		http.Error(w, "disk sync failed", 500)
		return
	}
	item.offset += n
	t.mu.Lock()
	t.tickets[id] = item
	t.mu.Unlock()
	w.Header().Set("Upload-Offset", strconv.FormatInt(item.offset, 10))
	if item.offset < item.total {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "accepted=%d next_offset=%d", n, item.offset)
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "checksum failed", 500)
		return
	}
	h := sha256.New()
	hashed, err := io.Copy(h, io.LimitReader(f, item.total+1))
	if err != nil || hashed != item.total {
		http.Error(w, "checksum failed", 500)
		return
	}
	if err = commitTransferFile(resource.parent, resource.staged, resource.target, item.overwrite); err != nil {
		if errors.Is(err, os.ErrExist) {
			http.Error(w, "destination already exists; upload did not overwrite it", 409)
		} else {
			http.Error(w, "finalize failed", 500)
		}
		return
	}
	t.forgetLocked(id)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintf(w, "saved_bytes=%d sha256=%s", item.total, hex.EncodeToString(h.Sum(nil)))
}
func (t *TransferManager) sweepExpired() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, item := range t.tickets {
		t.expireLocked(id, item)
	}
}
