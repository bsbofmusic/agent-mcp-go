package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Root confines all Patch IO, including staging and rollback. Pinned parent
// handles keep a later parent rename/symlink switch from redirecting a commit.
// This is filesystem confinement, not a cross-process compare-and-swap: another
// writer can still race the last version check and Rename/Remove.
type patchFileRef struct {
	root *os.Root
	name string
}

type patchSnapshot struct {
	info os.FileInfo
	data []byte
}

const (
	maxPatchWorkingBytes int64 = 16 << 20
	maxPatchWorkingLines       = 100000
)

var errPatchBudget = errors.New("patch working set exceeds 16 MiB or 100000 lines; use transfer or smaller patches")

// snapshotPatchFile reads at most limit+1 bytes even if the file grows after
// Stat. It checks the opened inode and the directory entry on both sides of the
// read, refusing leaf symlinks and obvious concurrent replacement/modification.
func snapshotPatchFile(ref *patchFileRef, limit int64) (patchSnapshot, error) {
	info, err := ref.root.Lstat(ref.name)
	if err != nil {
		return patchSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return patchSnapshot{}, fmt.Errorf("target is not a regular file: %s", ref.name)
	}
	if info.Size() > limit {
		return patchSnapshot{}, errPatchBudget
	}
	file, opened, err := openRootRegularFile(ref.root, ref.name, info)
	if err != nil {
		return patchSnapshot{}, err
	}
	defer file.Close()
	if opened.Mode() != info.Mode() {
		return patchSnapshot{}, fmt.Errorf("patch source changed: %s", ref.name)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return patchSnapshot{}, err
	}
	if int64(len(data)) > limit {
		return patchSnapshot{}, errPatchBudget
	}
	end, err := file.Stat()
	if err != nil {
		return patchSnapshot{}, err
	}
	entry, err := ref.root.Lstat(ref.name)
	if err != nil {
		return patchSnapshot{}, err
	}
	if !os.SameFile(info, end) || !os.SameFile(info, entry) || info.Mode() != end.Mode() || info.Mode() != entry.Mode() ||
		info.Size() != end.Size() || end.Size() != int64(len(data)) || !info.ModTime().Equal(end.ModTime()) {
		return patchSnapshot{}, fmt.Errorf("patch source changed during read: %s", ref.name)
	}
	return patchSnapshot{info: end, data: data}, nil
}

func (ref *patchFileRef) check(want patchSnapshot) error {
	got, err := snapshotPatchFile(ref, int64(len(want.data)))
	if err != nil {
		return fmt.Errorf("patch version conflict for %s: %w", ref.name, err)
	}
	if !os.SameFile(want.info, got.info) || want.info.Mode() != got.info.Mode() || !bytes.Equal(want.data, got.data) {
		return fmt.Errorf("patch version conflict for %s (identity, bytes or mode changed)", ref.name)
	}
	return nil
}

func (ref *patchFileRef) absent() error {
	_, err := ref.root.Lstat(ref.name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("patch destination already exists: %s", ref.name)
}

// stagePatchFile uses exclusive creation within a pinned parent. The returned
// identity belongs to the staged inode, so a concurrent writer cannot be
// accidentally adopted as our committed version after Rename/Link.
func stagePatchFile(ref *patchFileRef, data []byte, mode os.FileMode) (string, patchSnapshot, error) {
	name := ".agent-mcp-patch-" + rand.Text()
	file, err := ref.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", patchSnapshot{}, err
	}
	var info os.FileInfo
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		info, err = file.Stat()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		removeErr := ref.root.Remove(name)
		return "", patchSnapshot{}, errors.Join(err, removeErr)
	}
	return name, patchSnapshot{info: info, data: data}, nil
}

// patchParents owns the lifetime of pinned source/destination directories.
// No method falls back to an absolute path after opening the work root.
type patchParents struct {
	work *os.Root
	open map[string]*os.Root
}

func (p *patchParents) close() {
	for _, root := range p.open {
		_ = root.Close()
	}
	_ = p.work.Close()
}

func (p *patchParents) ref(path string, create bool) (*patchFileRef, error) {
	parent := filepath.Dir(path)
	root := p.open[parent]
	if root == nil {
		if create {
			if err := p.work.MkdirAll(parent, 0700); err != nil {
				return nil, err
			}
		}
		var err error
		root, err = p.work.OpenRoot(parent)
		if err != nil {
			return nil, err
		}
		p.open[parent] = root
	}
	return &patchFileRef{root: root, name: filepath.Base(path)}, nil
}

// patchRemoveFile keeps fault injection at the rooted operation boundary.
var patchRemoveFile = func(ref *patchFileRef) error { return ref.root.Remove(ref.name) }

func commitPatchChange(ch *stagedChange) error {
	if ch.existed {
		if err := ch.source.check(ch.original); err != nil {
			return err
		}
	}
	if ch.kind == "delete" {
		if err := patchRemoveFile(ch.source); err != nil {
			return err
		}
		ch.sourceRemoved = true
		return nil
	}
	stage := &patchFileRef{root: ch.destination.root, name: ch.temp}
	if err := stage.check(ch.committed); err != nil {
		return err
	}
	if ch.kind == "add" || ch.moveTo != ch.path {
		// Link provides atomic no-clobber even if another writer creates the
		// destination after preflight or our final version check.
		if err := ch.destination.root.Link(ch.temp, ch.destination.name); err != nil {
			return err
		}
	} else {
		if err := ch.source.check(ch.original); err != nil {
			return err
		}
		if err := ch.destination.root.Rename(ch.temp, ch.destination.name); err != nil {
			return err
		}
	}
	ch.wrote = true
	if ch.moveTo != ch.path {
		if err := ch.source.check(ch.original); err != nil {
			return err
		}
		if err := patchRemoveFile(ch.source); err != nil {
			return err
		}
		ch.sourceRemoved = true
	}
	return nil
}

// restorePatchFile stages the original payload and rechecks the destination at
// the last possible moment. Deleted sources are restored with atomic no-clobber.
func restorePatchFile(ref *patchFileRef, original patchSnapshot, current *patchSnapshot) (err error) {
	if current == nil {
		err = ref.absent()
	} else {
		err = ref.check(*current)
	}
	if err != nil {
		return err
	}
	name, _, err := stagePatchFile(ref, original.data, original.info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if e := ref.root.Remove(name); e != nil && !os.IsNotExist(e) {
			err = errors.Join(err, e)
		}
	}()
	if current == nil {
		return ref.root.Link(name, ref.name)
	}
	if err := ref.check(*current); err != nil {
		return err
	}
	return ref.root.Rename(name, ref.name)
}

func rollbackPatchChange(ch *stagedChange) error {
	if ch.kind == "delete" {
		if !ch.sourceRemoved {
			return nil
		}
		return restorePatchFile(ch.source, ch.original, nil)
	}
	if !ch.wrote {
		return nil
	}
	if err := ch.destination.check(ch.committed); err != nil {
		return err
	}
	if ch.kind == "add" {
		return patchRemoveFile(ch.destination)
	}
	if ch.moveTo == ch.path {
		return restorePatchFile(ch.source, ch.original, &ch.committed)
	}
	if ch.sourceRemoved {
		if err := restorePatchFile(ch.source, ch.original, nil); err != nil {
			return err
		}
	} else if err := ch.source.check(ch.original); err != nil {
		return err
	}
	// This also handles a partial Move whose source removal failed. Any
	// destination-cleanup failure is surfaced by the common rollback report.
	if err := ch.destination.check(ch.committed); err != nil {
		return err
	}
	return patchRemoveFile(ch.destination)
}
