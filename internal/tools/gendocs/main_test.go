package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// The Homebrew cask installs the manual pages it lists by name, so the list
// in .goreleaser.yaml must name exactly the pages generated for the command
// tree: a new command adds a page, a removed or renamed one drops it.
func TestHomebrewCaskListsEveryManualPage(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	if err := generate(out, "1.2.3", time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	generated, err := filepath.Glob(filepath.Join(out, "man", "*.1"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, page := range generated {
		want = append(want, "man/"+filepath.Base(page))
	}
	slices.Sort(want)

	config, err := os.ReadFile(filepath.Join("..", "..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := caskManpages(config)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("homebrew_casks manpages in .goreleaser.yaml:\n  %s\nwant the generated pages:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// caskManpages returns the items of the manpages list under homebrew_casks.
func caskManpages(config []byte) []string {
	var pages []string
	inCasks, inList := false, false
	sc := bufio.NewScanner(bytes.NewReader(config))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			inCasks, inList = strings.HasPrefix(line, "homebrew_casks:"), false
		case inCasks && trimmed == "manpages:":
			inList = true
		case inList && strings.HasPrefix(trimmed, "- "):
			pages = append(pages, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
		case inList && trimmed != "" && !strings.HasPrefix(trimmed, "#"):
			inList = false
		}
	}
	return pages
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
