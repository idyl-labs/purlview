package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateWritesCompletionsAndManualPages(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	when := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	if err := generate(out, "1.2.3", when); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"completions/purlview.bash",
		"completions/_purlview",
		"completions/purlview.fish",
		"completions/purlview.ps1",
		"man/purlview.1",
		"man/purlview-share.1",
		"man/purlview-completion-bash.1",
		"man-gz/purlview.1.gz",
	} {
		st, err := os.Stat(filepath.Join(out, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("%s is empty", rel)
		}
	}
	page, err := os.ReadFile(filepath.Join(out, "man/purlview.1"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"Purlview 1.2.3"`; !contains(page, want) {
		t.Errorf("manual page header missing %s:\n%s", want, page)
	}
	if !contains(page, "Sep 2026") {
		t.Errorf("manual page must carry the given date:\n%s", page)
	}
}

func contains(b []byte, s string) bool {
	return len(s) == 0 || len(b) > 0 && indexOf(string(b), s) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
