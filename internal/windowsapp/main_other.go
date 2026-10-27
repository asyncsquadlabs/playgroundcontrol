//go:build !windows

package windowsapp

import (
	"fmt"
	"os"
)

// Main exists so the portable packages can be built and tested on non-Windows
// development hosts. The deliverable runs on Windows 10/11 x64.
func Main() {
	fmt.Fprintln(os.Stderr, "Playground Control is a Windows 10/11 x64 application.")
	os.Exit(1)
}
