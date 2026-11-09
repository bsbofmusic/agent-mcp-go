package main

import (
	"errors"
	"os"
	"path/filepath"
)

// Keep the opened parent as the authority through staging and rename. A parent
// symlink changed after validation cannot redirect this write outside WorkRoot.
func atomicWorkspaceFile(c Config, path string, data []byte) error {
	root, rel, err := openWorkspaceRoot(c, path)
	if err != nil {
		return err
	}
	defer root.Close()
	directory, name := filepath.Dir(rel), filepath.Base(rel)
	if err := root.MkdirAll(directory, 0700); err != nil {
		return err
	}
	parent, err := root.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer parent.Close()
	mode := os.FileMode(0600)
	if info, err := parent.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("write target must be a regular file")
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	staged := ".agent-mcp-write-" + id
	file, err := parent.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer parent.Remove(staged)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return parent.Rename(staged, name)
}
