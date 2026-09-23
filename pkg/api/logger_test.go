package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewAPILoggerCreatesLogFile(t *testing.T) {
	dir := t.TempDir()
	l, err := newAPILogger(dir, false)
	if err != nil {
		t.Fatalf("newAPILogger: %v", err)
	}
	l.printf("hello %s", "world")
	l.warnf("warning %d", 42)

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one log file, got %d", len(files))
	}
	path := filepath.Join(dir, files[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "hello world") {
		t.Errorf("log missing printf message: %s", content)
	}
	if !strings.Contains(content, "warning 42") {
		t.Errorf("log missing warnf message: %s", content)
	}
}

func TestNewAPILoggerEmptyDir(t *testing.T) {
	l, err := newAPILogger("", false)
	if err != nil {
		t.Fatalf("newAPILogger: %v", err)
	}
	l.printf("no file")
	l.warnf("still no file")
}

func TestSummarize(t *testing.T) {
	if got := summarize("body", "hi", 10); !strings.Contains(got, "body=2") {
		t.Errorf("unexpected summary for short text: %s", got)
	}
	long := strings.Repeat("a", 100)
	if got := summarize("body", long, 10); !strings.Contains(got, "body=100") {
		t.Errorf("unexpected summary for long text: %s", got)
	}
	if got := summarize("body", long, 10); strings.Contains(got, long) {
		t.Errorf("long text should be truncated: %s", got)
	}
}
