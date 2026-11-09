package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// workspaceRelativePath validates lexical containment before any filesystem IO.
// os.Root then enforces containment while resolving symlinks, including races.
func workspaceRelativePath(c Config, absPath string) (string, error) {
	if !filepath.IsAbs(absPath) {
		return "", errors.New("workspace path must be absolute")
	}
	rel, err := filepath.Rel(filepath.Clean(c.WorkRoot), filepath.Clean(absPath))
	if err != nil || !filepath.IsLocal(rel) {
		return "", errors.New("path escapes configured work_root")
	}
	return rel, nil
}

func openWorkspaceRoot(c Config, absPath string) (*os.Root, string, error) {
	rel, err := workspaceRelativePath(c, absPath)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(c.WorkRoot)
	return root, rel, err
}

func openWorkspaceFile(c Config, absPath string) (*os.File, error) {
	root, rel, err := openWorkspaceRoot(c, absPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	// The returned file owns an independent descriptor and survives root.Close.
	return root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// openRootRegularFile cannot block waiting for a FIFO peer if a regular entry
// is replaced after inspection. Validate the opened descriptor before any read;
// checking the earlier path or DirEntry alone does not validate this inode.
func openRootRegularFile(root *os.Root, name string, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err == nil && (!opened.Mode().IsRegular() || expected == nil || !os.SameFile(expected, opened)) {
		err = &os.PathError{Op: "open regular file", Path: name, Err: errors.New("file is not regular or changed after inspection")}
	}
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, opened, nil
}

func readWorkspaceDirectory(c Config, absPath string) ([]os.DirEntry, error) {
	file, err := openWorkspaceFile(c, absPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	// File.ReadDir uses directory order; keep the os.ReadDir name ordering.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

func statWorkspacePath(c Config, absPath string) (os.FileInfo, error) {
	root, rel, err := openWorkspaceRoot(c, absPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Stat(rel)
}
