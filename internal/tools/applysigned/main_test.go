package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNoSignedDirectoryIsANoOp(t *testing.T) {
	t.Parallel()
	built := filepath.Join(t.TempDir(), "purlview")
	write(t, built, "unsigned")
	var out bytes.Buffer
	if err := run([]string{built, "darwin", "arm64"}, env(nil), &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(built); string(got) != "unsigned" {
		t.Fatalf("binary was modified: %q", got)
	}
	if !strings.Contains(out.String(), "keeping unsigned build") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestAppliesSignedBinaryWhenRebuildMatches(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	built := filepath.Join(dir, "build", "purlview.exe")
	write(t, built, "unsigned bytes")
	hash, err := fileSHA256(built)
	if err != nil {
		t.Fatal(err)
	}
	signedRoot := filepath.Join(dir, "signed")
	write(t, filepath.Join(signedRoot, "windows_amd64", "purlview.exe"), "signed bytes")
	write(t, filepath.Join(signedRoot, "windows_amd64", "unsigned.sha256"), hash+"  purlview.exe\n")

	var out bytes.Buffer
	if err := run([]string{built, "windows", "amd64"}, env(map[string]string{"PURLVIEW_SIGNED_BINARIES": signedRoot}), &out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(built)
	if string(got) != "signed bytes" {
		t.Fatalf("binary is %q, want the signed bytes", got)
	}
	if st, _ := os.Stat(built); runtime.GOOS != "windows" && st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("applied binary is not executable: %v", st.Mode())
	}
}

func TestRefusesWhenRebuildDiffersFromSignedInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	built := filepath.Join(dir, "build", "purlview")
	write(t, built, "a different rebuild")
	signedRoot := filepath.Join(dir, "signed")
	write(t, filepath.Join(signedRoot, "darwin_arm64", "purlview"), "signed bytes")
	write(t, filepath.Join(signedRoot, "darwin_arm64", "unsigned.sha256"), strings.Repeat("0", 64)+"  purlview\n")

	err := run([]string{built, "darwin", "arm64"}, env(map[string]string{"PURLVIEW_SIGNED_BINARIES": signedRoot}), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "does not match the binary that was signed") {
		t.Fatalf("expected a mismatch error, got %v", err)
	}
	if got, _ := os.ReadFile(built); string(got) != "a different rebuild" {
		t.Fatalf("binary must be untouched after a refusal, got %q", got)
	}
}

func TestMissingSignedBinaryPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	built := filepath.Join(dir, "purlview")
	write(t, built, "unsigned")
	signedRoot := filepath.Join(dir, "signed")
	if err := os.MkdirAll(signedRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		goos, allow string
		wantErr     bool
	}{
		{"linux", "", false},
		{"darwin", "", true},
		{"windows", "", true},
		{"darwin", "true", false},
		{"windows", "true", false},
		{"windows", "windows", false},
		{"darwin", "windows", true},
		{"linux", "windows", false},
		{"darwin", "false", true},
	}
	for _, tc := range tests {
		err := run([]string{built, tc.goos, "amd64"}, env(map[string]string{
			"PURLVIEW_SIGNED_BINARIES": signedRoot,
			"PURLVIEW_ALLOW_UNSIGNED":  tc.allow,
		}), &bytes.Buffer{})
		if (err != nil) != tc.wantErr {
			t.Errorf("goos=%s allow=%q: err=%v, wantErr=%v", tc.goos, tc.allow, err, tc.wantErr)
		}
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	if err := run(nil, env(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("expected a usage error")
	}
}
