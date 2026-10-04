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

package scenario

import (
	"strings"
	"testing"
)

// TestScenarios runs every scenario against a fresh world and reports each
// failed expectation with its transcript.
func TestScenarios(t *testing.T) {
	t.Parallel()
	for _, sc := range All() {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			o := Execute(sc)
			if !o.Passed() {
				for _, f := range o.Failures {
					t.Error(f)
				}
				t.Logf("transcript:\n%s", RenderTranscript(o.Transcript))
			}
		})
	}
}

func TestEveryScenarioHasAreaTitleAndSetup(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, sc := range All() {
		if seen[sc.Name] {
			t.Errorf("duplicate scenario %q", sc.Name)
		}
		seen[sc.Name] = true
		if !strings.Contains(sc.Name, "/") || sc.Title == "" || sc.Setup == "" || sc.Run == nil {
			t.Errorf("scenario %q needs area/name, a title, a setup and a body", sc.Name)
		}
	}
}

// A further app that does not answer is named however the CLI learns of the
// end. When the probes finish before the daemon answers the start,
// the reply already carries the end and the CLI never waits for the ended
// event, so the record itself must name the app, not the first one.
func TestUnreachableFurtherAppIsNamedWhenTheStartReplyCarriesTheEnd(t *testing.T) {
	t.Parallel()
	w := NewWorld()
	defer w.Daemon.Stop()
	w.SignIn()
	w.Prober.Unreachable("http://localhost:8000", "connection refused")
	w.Daemon.ReplyToNextStartAfterItEnds()
	r := w.Exec("share", "5173", "8000")
	if r.Code != 1 || r.Stdout != "" || r.Stderr != "✗ Nothing is answering at localhost:8000 — start your app, then share again\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.Code, r.Stdout, r.Stderr)
	}
	if creates, _ := w.Platform.Calls(); creates != 0 {
		t.Fatalf("no platform create may happen, got %d", creates)
	}
}
