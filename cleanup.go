package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func cleanupOrphanExports(stateDir string) error {
	dir := filepath.Join(stateDir, "exports")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.Type().IsRegular() && strings.HasPrefix(ent.Name(), "snapshot-") {
			if err := os.Remove(filepath.Join(dir, ent.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
