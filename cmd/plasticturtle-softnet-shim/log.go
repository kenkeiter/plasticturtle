package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// logger writes optional debug lines to the file named by PT_SHIM_LOG. When that
// is unset the shim is silent (it has no stdout of its own to spare — stdout may
// be Tart's control-fd channel). Appends are serialized so concurrent relay
// goroutines do not interleave.
type logger struct {
	mu   sync.Mutex
	path string
}

func newLogger(path string) *logger { return &logger{path: path} }

func (l *logger) enabled() bool { return l.path != "" }

func (l *logger) printf(format string, args ...any) {
	if l.path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [%d] %s\n", time.Now().Format("15:04:05.000"), os.Getpid(), fmt.Sprintf(format, args...))
}
