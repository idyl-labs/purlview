// Command semvercmp compares two semantic versions for release scripts.
//
//	semvercmp A B   prints -1, 0 or 1 and exits 0; exits 2 on a malformed input
//
// A leading "v" is ignored. Prereleases order before the release they precede
// (0.1.0-alpha.2 < 0.1.0-rc.1 < 0.1.0), per SemVer 2.0.0 item 11. Build
// metadata after "+" is ignored. The comparison lives in internal/semver and
// is shared with the CLI's update checker.
package main

import (
	"fmt"
	"os"

	"github.com/idyl-labs/purlview/internal/semver"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: semvercmp A B")
		os.Exit(2)
	}
	c, err := semver.CompareStrings(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "semvercmp:", err)
		os.Exit(2)
	}
	fmt.Println(c)
}
