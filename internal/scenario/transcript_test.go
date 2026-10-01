package scenario_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idyl-labs/purlview/internal/scenario"
)

// TestTranscriptDocumentIsCurrent fails when docs/cli-scenarios.md no longer
// matches what the handlers do. Regenerate it with
// go run ./internal/tools/scenario doc -write docs/cli-scenarios.md.
func TestTranscriptDocumentIsCurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "docs", "cli-scenarios.md")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (generate it with the scenario tool)", path, err)
	}
	got := scenario.RenderDocument(scenario.ExecuteAll())
	normalise := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	if normalise(string(want)) != normalise(got) {
		tmp := filepath.Join(t.TempDir(), "cli-scenarios.md")
		_ = os.WriteFile(tmp, []byte(got), 0o600)
		t.Fatalf("%s is stale; regenerate with: go run ./internal/tools/scenario doc -write docs/cli-scenarios.md (fresh copy at %s)", path, tmp)
	}
}
