//go:build !windows

package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir returns a private directory whose socket path fits the kernel
// limit (t.TempDir can be long on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pu-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenCreatesPrivateSocketAndAcceptsSameUser(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	path := filepath.Join(dir, "d.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o, want 0600", st.Mode().Perm())
	}
	accepted := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			_ = c.Close()
		}
		accepted <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Dial(ctx, path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()
	if err := <-accepted; err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestDialRefusesSharedSocketOrDirectory(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	path := filepath.Join(dir, "d.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx, path); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("world-writable socket must be refused, got %v", err)
	}
	_ = os.Chmod(path, 0o600)

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx, path); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("shared directory must be refused, got %v", err)
	}
	_ = os.Chmod(dir, 0o700)
	if _, err := Listen(filepath.Join(dir, "sub-does-not-exist", "x.sock")); err == nil {
		t.Fatal("listen without a private parent must fail")
	}
	shared := filepath.Join(dir, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(shared, "x.sock")); err == nil || !strings.Contains(err.Error(), "accessible to other users") {
		t.Fatalf("listen in a shared directory must be refused, got %v", err)
	}
}

func TestDialRejectsNonSocketAndMissingPath(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(ctx, regular); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: %v", err)
	}
	if _, err := Dial(ctx, filepath.Join(dir, "missing.sock")); err == nil {
		t.Fatal("missing socket must fail")
	}
}
