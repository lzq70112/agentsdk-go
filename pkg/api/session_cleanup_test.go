package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellarlinkco/agentsdk-go/pkg/message"
)

func TestSessionEvictionCleansToolOutputDir(t *testing.T) {
	store := newHistoryStore(1, nil)
	sessionID := "session-to-evict"
	dir := toolOutputSessionDir(sessionID)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	toolDir := filepath.Join(dir, "echo")
	if err := os.MkdirAll(toolDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "stdout.output"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write dummy output: %v", err)
	}

	if _, err := store.Get(sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(100 * time.Microsecond)
	if _, err := store.Get("session-to-keep"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	ids := store.SessionIDs()
	if len(ids) != 1 || ids[0] != "session-to-keep" {
		t.Fatalf("expected store to retain only session-to-keep, got %v", ids)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected session dir removed after eviction, stat=%v", err)
	}
}

func TestSessionEvictionInvokesCallbackWhenPresent(t *testing.T) {
	store := newHistoryStore(1, nil)
	var evicted []string

	store.onEvict = func(id string) {
		evicted = append(evicted, id)
	}

	if _, err := store.Get("session-to-evict"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	time.Sleep(100 * time.Microsecond)
	if _, err := store.Get("session-to-keep"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(evicted) != 1 || evicted[0] != "session-to-evict" {
		t.Fatalf("evicted=%v, want [session-to-evict]", evicted)
	}
}

func TestRuntimeCloseCleansToolOutputDirs(t *testing.T) {
	rt := &Runtime{histories: newHistoryStore(0, nil)}

	sessions := []string{"sess-a", "sess-b"}
	for _, sessionID := range sessions {
		if _, err := rt.histories.Get(sessionID); err != nil {
			t.Fatalf("Get: %v", err)
		}
		dir := toolOutputSessionDir(sessionID)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })

		toolDir := filepath.Join(dir, "echo")
		if err := os.MkdirAll(toolDir, 0o700); err != nil {
			t.Fatalf("mkdir session dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(toolDir, "stdout.output"), []byte("ok"), 0o600); err != nil {
			t.Fatalf("write dummy output: %v", err)
		}
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, sessionID := range sessions {
		dir := toolOutputSessionDir(sessionID)
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected session dir %q removed, stat=%v", sessionID, err)
		}
	}
}

func TestCleanupToolOutputSessionDirIsIdempotent(t *testing.T) {
	sessionID := "missing-session"
	dir := toolOutputSessionDir(sessionID)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = os.RemoveAll(dir)

	if err := cleanupToolOutputSessionDir(sessionID); err != nil {
		t.Fatalf("cleanup missing dir: %v", err)
	}
	if err := cleanupToolOutputSessionDir(sessionID); err != nil {
		t.Fatalf("cleanup missing dir again: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected dir to remain absent, stat=%v", err)
	}
}

func TestHistoryStoreClear(t *testing.T) {
	store := newHistoryStore(10, nil)

	// 先创建一个 session
	hist, err := store.Get("sess-to-clear")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	hist.Append(message.Message{Role: "user", Content: "hello"})

	// 验证 session 存在
	if _, ok := store.Loaded("sess-to-clear"); !ok {
		t.Fatal("expected session to exist before clear")
	}

	// 清空
	if !store.Clear("sess-to-clear") {
		t.Fatal("expected Clear to return true")
	}

	// 验证 session 已从 data 和 lastUsed 中删除
	if _, ok := store.Loaded("sess-to-clear"); ok {
		t.Fatal("expected session to be removed after clear")
	}
	for _, id := range store.SessionIDs() {
		if id == "sess-to-clear" {
			t.Fatal("expected cleared session not in SessionIDs")
		}
	}
}

func TestHistoryStoreClearMissing(t *testing.T) {
	store := newHistoryStore(10, nil)

	if store.Clear("missing") {
		t.Fatal("expected Clear to return false for missing session")
	}
}

func TestHistoryStoreClearEmptyID(t *testing.T) {
	store := newHistoryStore(10, nil)
	if store.Clear("") {
		t.Fatal("expected Clear to return false for empty id")
	}
	if store.Clear("   ") {
		t.Fatal("expected Clear to return false for whitespace id")
	}
}

func TestHistoryStoreClearNilStore(t *testing.T) {
	var store *historyStore
	if store.Clear("anything") {
		t.Fatal("expected Clear on nil store to return false")
	}
}
