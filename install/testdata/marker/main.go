// Command marker stands in for a release's executable in the installer tests
// that must prove an executable never ran: run, it writes the file
// INSTALL_TEST_MARKER names, then answers --version as the release would.
package main

import (
	"fmt"
	"os"
)

var version = "0.0.0"

func main() {
	if path := os.Getenv("INSTALL_TEST_MARKER"); path != "" {
		_ = os.WriteFile(path, []byte("ran\n"), 0o600)
	}
	fmt.Println("purlview " + version)
}
