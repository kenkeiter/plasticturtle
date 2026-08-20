// Package progname reports the name Plastic Turtle was invoked as.
//
// The binary installs as `plasticturtle` and is symlinked to `turtle`, so any
// message that tells the user what to type next has two right answers and no
// way to pick one at compile time. Echoing argv[0] means the advice matches the
// command they actually ran, including from a symlink or alias of their own.
package progname

import (
	"os"
	"path/filepath"
	"strings"
)

// Default is used when argv[0] is empty or unusable — a caller that execs us
// with an empty argv, or a test binary whose name says nothing about us.
const Default = "plasticturtle"

// Get returns the base name of argv[0], or Default if that is not a plausible
// command name. Go test binaries are the common unusable case: they are named
// after the package under test (`plasticturtle.test`), which is not a command
// anyone can run, and letting that leak into golden output would make message
// assertions depend on how the test was built.
func Get() string {
	if len(os.Args) == 0 {
		return Default
	}
	base := filepath.Base(os.Args[0])
	if base == "." || base == string(filepath.Separator) || base == "" {
		return Default
	}
	if strings.HasSuffix(base, ".test") {
		return Default
	}
	return base
}
