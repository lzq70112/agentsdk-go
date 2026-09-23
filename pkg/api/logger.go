package api

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// apiLogger writes runtime diagnostics to an optional file sink and stderr.
// Routine diagnostics go to the file; warnings/errors are also mirrored to
// stderr so operators still see critical problems without the full stream.
//
// The logger opens the file for each write and closes it immediately so that
// tests and short-lived Runtimes do not hold handles that block temp-directory
// cleanup (notably on Windows).
type apiLogger struct {
	mu      sync.Mutex
	path    string
	verbose bool
}

// newAPILogger creates a logger that appends to <dir>/agentsdk-YYYYMMDD.log.
// If dir is empty no file is opened and logs are only written through warnf.
// Production callers typically set dir to <ProjectRoot>/.agents/logs.
func newAPILogger(dir string, verbose bool) (*apiLogger, error) {
	l := &apiLogger{verbose: verbose}
	if dir == "" {
		return l, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", dir, err)
	}
	l.path = filepath.Join(dir, fmt.Sprintf("agentsdk-%s.log", time.Now().Format("20060102")))
	return l, nil
}

// printf writes a routine diagnostic to the file sink, and to stderr only
// when verbose mode is enabled.
func (l *apiLogger) printf(format string, args ...any) {
	if l == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if l.verbose {
		log.Printf("%s", msg)
	}
	l.writeToFile(msg)
}

// warnf writes a warning or error to both the file sink and stderr.
func (l *apiLogger) warnf(format string, args ...any) {
	if l == nil {
		log.Printf(format, args...)
		return
	}
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	l.writeToFile(msg)
}

func (l *apiLogger) writeToFile(msg string) {
	if l.path == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), msg)
}

// runtimeLogger is the process-wide logging sink used by pkg/api. It is
// replaced once during Runtime construction; after that it is read-only.
//
// Because it is process-wide, the last constructed Runtime controls the log
// destination. Production deployments typically create one Runtime per process,
// so this is acceptable. Tests that create many Runtimes with different
// ProjectRoot values may share the same log file.
var runtimeLogger = &apiLogger{verbose: false}

// initRuntimeLogger configures the process-wide logger. Failures are logged to
// stderr but do not prevent runtime construction.
func initRuntimeLogger(dir string, verbose bool) {
	l, err := newAPILogger(dir, verbose)
	if err != nil {
		log.Printf("api: logger init failed: %v", err)
		return
	}
	runtimeLogger = l
}

// stringLen returns the length of s in characters (runes).
func stringLen(s string) int {
	return len([]rune(s))
}

// summarize returns a compact representation of a payload: rune count and,
// when small enough, the full text. The goal is to avoid dumping large
// contexts into logs while still keeping short values readable.
func summarize(label, s string, maxChars int) string {
	n := stringLen(s)
	if n <= maxChars {
		return fmt.Sprintf("%s=%d(%q)", label, n, s)
	}
	runes := []rune(s)
	return fmt.Sprintf("%s=%d(%q...%q)", label, n, string(runes[:maxChars/2]), string(runes[n-maxChars/2:]))
}
