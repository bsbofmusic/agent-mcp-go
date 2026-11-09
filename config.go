package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxPermittedFile = int64(1024 * 1024 * 1024)

type Config struct {
	Listen            string        `json:"listen"`
	WorkRoot          string        `json:"work_root"`
	SkillsRoot        string        `json:"skills_root"`
	StateDir          string        `json:"state_dir"`
	TokenFile         string        `json:"token_file"`
	PublicURL         string        `json:"public_url"`
	SharedMCPBin      string        `json:"shared_mcp_bin"`
	ExecutableDirs    []string      `json:"executable_dirs,omitempty"`
	Ignore            *IgnoreConfig `json:"ignore,omitempty"`
	MaxFileBytes      int64         `json:"max_file_bytes"`
	MaxOutputBytes    int           `json:"max_output_bytes"`
	CommandTimeoutSec int           `json:"command_timeout_seconds"`
}

type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func readConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.WorkRoot == "" || c.TokenFile == "" || c.StateDir == "" || c.Listen == "" {
		return c, errors.New("listen, work_root, state_dir and token_file are required")
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") {
		return c, errors.New("refusing non-loopback listener; use the existing HTTPS reverse proxy")
	}
	if err := validateTuning(c.MaxFileBytes, c.MaxOutputBytes, c.CommandTimeoutSec); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.WorkRoot) || !filepath.IsAbs(c.StateDir) {
		return c, errors.New("work_root and state_dir must be absolute paths")
	}
	for _, directory := range c.ExecutableDirs {
		if !filepath.IsAbs(directory) || strings.ContainsRune(directory, os.PathListSeparator) {
			return c, errors.New("executable_dirs must contain absolute directories without path-list separators")
		}
	}
	if c.Ignore != nil {
		if len(c.Ignore.CustomPatterns) > 4096 {
			return c, errors.New("too many custom ignore patterns")
		}
		bytes := 0
		for _, pattern := range c.Ignore.CustomPatterns {
			bytes += len(pattern)
		}
		if bytes > 1<<20 {
			return c, errors.New("custom ignore patterns exceed 1 MiB")
		}
	}
	return c, nil
}

func validateTuning(file int64, out int, timeout int) error {
	if file < 1<<20 || file > maxPermittedFile {
		return fmt.Errorf("max_file_bytes must be between 1 MiB and 1 GiB")
	}
	if out < 1024 || out > 4*1024*1024 {
		return fmt.Errorf("max_output_bytes must be between 1 KiB and 4 MiB")
	}
	if timeout < 1 || timeout > 3600 {
		return fmt.Errorf("command_timeout_seconds must be between 1 and 3600")
	}
	return nil
}

func newConfigStore(path string) (*ConfigStore, error) {
	c, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	return &ConfigStore{path: path, cfg: c}, nil
}
func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *ConfigStore) Update(file int64, output int, timeout int) (Config, error) {
	if err := validateTuning(file, output, timeout); err != nil {
		return Config{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	next.MaxFileBytes, next.MaxOutputBytes, next.CommandTimeoutSec = file, output, timeout
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Config{}, err
	}
	if err = atomicFile(s.path, append(b, '\n'), 0600); err != nil {
		return Config{}, err
	}
	s.cfg = next
	return next, nil
}

func atomicFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".agent-mcp-go-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func readBearerToken(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0077 != 0 {
		return nil, errors.New("bearer token file must be owner-only (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, errors.New("bearer token must have at least 32 characters")
	}
	return b, nil
}

func tokenEqual(got string, want []byte) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), want) == 1
}
