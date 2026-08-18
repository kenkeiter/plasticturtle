// Package deps pins the module's runtime dependencies so that go.mod stays
// stable while packages are still skeletons.
//
// Without this, `go mod tidy` drops any library that no implemented package
// imports yet, and each agent filling in a package would have to re-add it —
// racing every other agent on go.mod and go.sum. Delete this file once every
// package below is genuinely imported somewhere.
package deps

import (
	_ "github.com/charmbracelet/huh" // pt init image picker
	_ "github.com/gofrs/flock"       // project state locking
	_ "golang.org/x/crypto/ssh"      // sessions and tunnels
	_ "golang.org/x/sys/unix"        // KERN_PROC_PID liveness checks
	_ "golang.org/x/term"            // raw mode for interactive sessions
	_ "gopkg.in/yaml.v3"             // strict .plasticturtle decoding
)
