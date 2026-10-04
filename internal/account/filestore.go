// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package account

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/idyl-labs/purlview/sdk/api"
)

// FileStore keeps the credential in one private JSON file.
//
// The CLI owns this supported credential store. The daemon receives a
// credential over private IPC for a share's lifetime; it never reads the file.
// Production callers select the environment/endpoint with ScopedFileStore.
type FileStore struct {
	Path string
}

// NewFileStore returns a store at path; nothing is created until Save.
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

// Load reads the credential; a missing file means signed out.
func (s *FileStore) Load() (*api.InstallationCredential, error) {
	if st, err := os.Lstat(s.Path); err == nil {
		if !st.Mode().IsRegular() {
			return nil, errors.New("credential must be a regular file")
		}
		if err = privateFile(s.Path, false); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var c api.InstallationCredential
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path, err)
	}
	if c.Account == "" || c.Device == "" || c.Token == "" {
		return nil, fmt.Errorf("%s: incomplete credential; run 'purlview logout' to discard it", s.Path)
	}
	return &c, nil
}

// Save writes the credential atomically with private permissions.
func (s *FileStore) Save(c *api.InstallationCredential) error {
	if c == nil {
		return errors.New("save: nil credential")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := privateFile(filepath.Dir(s.Path), true); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".credential-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = privateFile(tmp, true); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, s.Path); err != nil {
		return err
	}

	return nil
}

// Clear removes the credential file; an absent file is not an error.
func (s *FileStore) Clear() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// MemoryStore keeps the credential in memory (scenarios and tests).
type MemoryStore struct {
	cred *api.InstallationCredential
}

// Load returns the remembered credential.
func (s *MemoryStore) Load() (*api.InstallationCredential, error) {
	if s.cred == nil {
		return nil, nil
	}
	c := *s.cred
	return &c, nil
}

// Save remembers a copy.
func (s *MemoryStore) Save(c *api.InstallationCredential) error {
	cp := *c
	s.cred = &cp
	return nil
}

// Clear forgets the credential.
func (s *MemoryStore) Clear() error {
	s.cred = nil
	return nil
}

// ScopedFileStore deliberately never falls back to an unscoped legacy token.
func ScopedFileStore(base, environment, endpoint, host string) *FileStore {
	if endpoint == "" {
		endpoint = "unconfigured"
	}
	key := sha256.Sum256([]byte(environment + "\x00" + endpoint + "\x00" + host))
	return NewFileStore(base + "." + hex.EncodeToString(key[:16]))
}
