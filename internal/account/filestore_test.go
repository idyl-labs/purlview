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
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

func TestFileStoreRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "credentials.json")
	s := NewFileStore(path)
	if c, err := s.Load(); err != nil || c != nil {
		t.Fatalf("empty store: %v %v", c, err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("clear on empty store: %v", err)
	}
	want := &api.InstallationCredential{Identity: resource.Identity{Account: "creator@example.invalid", AccountID: "acc_1", Device: "dev_1", DeviceLabel: "studio"}, Token: "secret", IssuedAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o, want 0600", st.Mode().Perm())
		}
		dst, _ := os.Stat(filepath.Dir(path))
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %o, want 0700", dst.Mode().Perm())
		}
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("temporary file left behind")
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Load(); err != nil || c != nil {
		t.Fatalf("after clear: %v %v", c, err)
	}
}

func TestFileStoreRejectsIncompleteFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"account":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path).Load(); err == nil {
		t.Fatal("incomplete credential must be reported")
	}
	if err := os.WriteFile(path, []byte(`{bogus`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(path).Load(); err == nil {
		t.Fatal("malformed credential must be reported")
	}
}

func TestFailureKinds(t *testing.T) {
	t.Parallel()
	err := &Failure{Kind: KindDenied, Detail: "the user declined"}
	if !IsKind(err, KindDenied) || IsKind(err, KindExpired) {
		t.Fatal("IsKind")
	}
	if err.Error() != "denied: the user declined" || (&Failure{Kind: KindExpired}).Error() != "expired" {
		t.Fatalf("message %q", err.Error())
	}
}

func TestCredentialScopeDoesNotFallBackOrCrossEndpoints(t *testing.T) {
	base := filepath.Join(t.TempDir(), "credentials.json")
	credential := &api.InstallationCredential{Identity: resource.Identity{Account: "creator@example.invalid", Device: "dev_test"}, Token: "test-token"}
	if err := NewFileStore(base).Save(credential); err != nil {
		t.Fatal(err)
	}
	first := ScopedFileStore(base, "dev", "https://account.one.invalid", "")
	if got, err := first.Load(); err != nil || got != nil {
		t.Fatal("scoped store fell back to unscoped credentials")
	}
	if err := first.Save(credential); err != nil {
		t.Fatal(err)
	}
	for _, other := range []*FileStore{ScopedFileStore(base, "dev", "https://account.two.invalid", ""), ScopedFileStore(base, "prod", "https://account.one.invalid", "")} {
		if got, err := other.Load(); err != nil || got != nil {
			t.Fatal("credential escaped its explicit environment/endpoint")
		}
	}
	if err := first.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, err := NewFileStore(base).Load(); err != nil || got == nil {
		t.Fatal("scoped logout removed an unrelated credential")
	}
}

func TestCredentialLoadRefusesUnsafeUnixFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission and symlink semantics")
	}
	base := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(base, []byte(`{"account":"a","device":"d","token":"t"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(base).Load(); err == nil {
		t.Fatal("broad credential permissions accepted")
	}
	if err := os.Chmod(base, 0600); err != nil {
		t.Fatal(err)
	}
	link := base + "-link"
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(link).Load(); err == nil {
		t.Fatal("credential symlink accepted")
	}
}
