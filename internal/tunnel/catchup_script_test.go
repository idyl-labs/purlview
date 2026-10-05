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

package tunnel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The script is small, and the stamp cannot end the element it is in.
func TestTheScriptTagIsSmallAndSelfContained(t *testing.T) {
	if n := len(catchupScript); n >= 2048 {
		t.Fatalf("the minified script is %d bytes", n)
	}
	tag := string(scriptTag(stamp{known: true, fw: vite, generation: 42, revision: 7}, "/_purlview/t/2"))
	if want := `<script data-purlview="catch-up">` + catchupScript + `({"g":"42","r":7,"u":"/_purlview/t/2/","p":["vite-hmr"]})</script>`; tag != want {
		t.Fatalf("tag %q", tag)
	}
	if tag := string(scriptTag(stamp{fw: &framework{subprotocol: "</script><b>"}}, "")); !strings.Contains(tag, `"g":null`) || strings.Count(tag, "</script>") != 1 {
		t.Fatalf("unknown stamp %q", tag)
	}
	// The script names no address, makes its one request through fetch, and
	// acts only through location.reload().
	for _, absent := range []string{"http", "//", "XMLHttpRequest", "sendBeacon", "postMessage", "import", "eval", "Function(", "innerHTML", "createElement"} {
		if strings.Contains(catchupScript, absent) {
			t.Errorf("the script contains %q", absent)
		}
	}
	if strings.Count(catchupScript, "location.reload()") != 2 || strings.Count(catchupScript, "F(") != 1 {
		t.Error("the script reloads or fetches in unexpected places")
	}
}

// The script's decisions, run in Node against a simulated page, for both the
// minified form the daemon inserts and the readable source beside it. The
// tests need nothing beyond Go, so this one is skipped where Node is not
// installed; GitHub's hosted runners have it.
func TestTheScriptReloadsOnlyWhenThePageMissedSomething(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	harness, err := filepath.Abs(filepath.Join("testdata", "catchup-harness.js"))
	if err != nil {
		t.Fatal(err)
	}
	minified := filepath.Join(t.TempDir(), "catchup.min.js")
	if err := os.WriteFile(minified, []byte(catchupScript), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"minified": minified, "readable": "catchup.js"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, node, harness, script).CombinedOutput()
		cancel()
		if err != nil {
			t.Errorf("%s script: %v\n%s", name, err, out)
		} else {
			t.Logf("%s script:\n%s", name, out)
		}
	}
}
