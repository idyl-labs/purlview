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

package scenario_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idyl-labs/purlview/internal/scenario"
)

// TestTranscriptDocumentIsCurrent fails when testdata/transcripts.md no longer
// matches what the handlers do. Regenerate it with
// go run ./internal/tools/scenario doc -write internal/scenario/testdata/transcripts.md.
func TestTranscriptDocumentIsCurrent(t *testing.T) {
	t.Parallel()
	path := filepath.Join("testdata", "transcripts.md")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (generate it with the scenario tool)", path, err)
	}
	got := scenario.RenderDocument(scenario.ExecuteAll())
	normalise := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	if normalise(string(want)) != normalise(got) {
		tmp := filepath.Join(t.TempDir(), "transcripts.md")
		_ = os.WriteFile(tmp, []byte(got), 0o600)
		t.Fatalf("%s is stale; regenerate with: go run ./internal/tools/scenario doc -write internal/scenario/testdata/transcripts.md (fresh copy at %s)", path, tmp)
	}
}
