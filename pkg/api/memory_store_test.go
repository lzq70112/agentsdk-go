package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// writeMemoryFile 在 dir 下写一个 .md 记忆文件并把 mtime 回拨 ageDays 天。
// 回拨时额外加 2 小时缓冲，避免 24 小时边界附近的天龄抖动（age=N 要求
// 严格落在 [N*24h, (N+1)*24h) 区间内）。
func writeMemoryFile(t *testing.T, dir, name, frontmatter, body string, ageDays int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	content := frontmatter + "\n" + body
	if frontmatter == "" {
		content = body
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if ageDays > 0 {
		past := time.Now().Add(-time.Duration(ageDays)*24*time.Hour - 2*time.Hour)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	return path
}

// memoryDoc 构造标准 frontmatter 头（typ 可省略，验证缺省 reference 语义）。
func memoryDoc(name, description string, typ ...string) string {
	doc := "---\nname: " + name + "\ndescription: " + description
	if len(typ) > 0 && typ[0] != "" {
		doc += "\ntype: " + typ[0]
	}
	doc += "\n---\n"
	return doc
}

func TestMemoryStoreScanParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeMemoryFile(t, dir, "user_role.md", memoryDoc("user role", "  用户   角色说明  ", "user"), "body", 0)
	writeMemoryFile(t, dir, "defaults.md", memoryDoc("defaults", "默认 type 缺省", ""), "body", 0)
	writeMemoryFile(t, dir, "no_description.md", "---\nname: nd\n---\nbody", "", 0)
	writeMemoryFile(t, dir, "empty_description.md", "---\nname: ed\ndescription: \"   \"\n---\nbody", "", 0)
	writeMemoryFile(t, dir, "disabled_false.md", memoryDoc("df", "禁用 false", "feedback"), "body", 0)
	if err := os.WriteFile(filepath.Join(dir, "disabled_false.md"), []byte("---\nname: df\ndescription: 禁用\ntype: feedback\nenabled: false\n---\nbody"), 0o600); err != nil {
		t.Fatalf("write disabled_false.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disabled_no.md"), []byte("---\nname: dn\ndescription: 禁用 no\nenabled: No\n---\nbody"), 0o600); err != nil {
		t.Fatalf("write disabled_no.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disabled_zero.md"), []byte("---\nname: dz\ndescription: 禁用 0\nenabled: 0\n---\nbody"), 0o600); err != nil {
		t.Fatalf("write disabled_zero.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "enabled_on.md"), []byte("---\nname: eo\ndescription: 启用 on\nenabled: yes\n---\nbody"), 0o600); err != nil {
		t.Fatalf("write enabled_on.md: %v", err)
	}

	store := &memoryStore{autoDir: dir}
	headers := store.scan("")
	if len(headers) != 3 {
		t.Fatalf("scanned %d headers, want 3 (valid ones only): %+v", len(headers), headers)
	}
	byName := map[string]memoryHeader{}
	for _, h := range headers {
		byName[filepath.Base(h.Filename)] = h
	}
	ur, ok := byName["user_role.md"]
	if !ok {
		t.Fatal("user_role.md missing")
	}
	if ur.Type != "user" {
		t.Fatalf("user_role type = %q, want user", ur.Type)
	}
	if ur.Description != "用户 角色说明" {
		t.Fatalf("description = %q, want whitespace-folded %q", ur.Description, "用户 角色说明")
	}
	if ur.Source != memorySourceAuto {
		t.Fatalf("auto source = %q, want %q", ur.Source, memorySourceAuto)
	}
	def, ok := byName["defaults.md"]
	if !ok {
		t.Fatal("defaults.md missing")
	}
	if def.Type != "reference" {
		t.Fatalf("missing type must default to reference, got %q", def.Type)
	}
	if _, ok := byName["no_description.md"]; ok {
		t.Fatal("frontmatter without description must be skipped")
	}
	if _, ok := byName["empty_description.md"]; ok {
		t.Fatal("blank description must be skipped")
	}
	for _, name := range []string{"disabled_false.md", "disabled_no.md", "disabled_zero.md"} {
		if _, ok := byName[name]; ok {
			t.Fatalf("%s must be explicitly disabled", name)
		}
	}
	if _, ok := byName["enabled_on.md"]; !ok {
		t.Fatal("enabled: yes must stay active")
	}
}

func TestMemoryStoreScanExcludesMemoryMdAndNonUTF8(t *testing.T) {
	dir := t.TempDir()
	writeMemoryFile(t, dir, "MEMORY.md", memoryDoc("index", "索引文件"), "body", 0)
	writeMemoryFile(t, dir, "memory.md", memoryDoc("index2", "小写索引"), "body", 0)
	writeMemoryFile(t, dir, "valid.md", memoryDoc("ok", "有效文件"), "body", 0)
	// 非 UTF-8（GBK 编码的中文）
	if err := os.WriteFile(filepath.Join(dir, "gbk.md"), []byte("---\nname: gbk\ndescription: \xD6\xD0\xCE\xC4\n---\nbody"), 0o600); err != nil {
		t.Fatalf("write gbk: %v", err)
	}

	store := &memoryStore{autoDir: dir}
	headers := store.scan("")
	if len(headers) != 1 {
		t.Fatalf("scanned %d headers, want 1: %+v", len(headers), headers)
	}
	if filepath.Base(headers[0].Filename) != "valid.md" {
		t.Fatalf("unexpected header %q", headers[0].Filename)
	}
}

func TestMemoryStoreScanCapsTwoHundredByMtime(t *testing.T) {
	dir := t.TempDir()
	// 205 个文件：mtime 越新编号越大；只保留最新的 200 个。
	for i := 0; i < 205; i++ {
		name := fmt.Sprintf("memo_%03d.md", i)
		writeMemoryFile(t, dir, name, memoryDoc(name, "记忆 "+fmt.Sprint(i)), "body", 0)
		age := time.Duration(205-i) * time.Minute // 越小越新
		past := time.Now().Add(-age)
		if err := os.Chtimes(filepath.Join(dir, name), past, past); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}

	store := &memoryStore{autoDir: dir}
	headers := store.scan("")
	if len(headers) != memoryMaxScanFileCount {
		t.Fatalf("scanned %d headers, want cap %d", len(headers), memoryMaxScanFileCount)
	}
	// 最新 5 个应是 memo_204..memo_200。
	want := []string{"memo_204.md", "memo_203.md", "memo_202.md", "memo_201.md", "memo_200.md"}
	for i, h := range headers[:5] {
		if filepath.Base(h.Filename) != want[i] {
			t.Fatalf("header[%d] = %q, want %q (newest first)", i, h.Filename, want[i])
		}
	}
}

func TestMemoryStoreScanExtraDirsPrefixAndDedup(t *testing.T) {
	auto := t.TempDir()
	extra1 := t.TempDir()
	extra2 := t.TempDir()
	writeMemoryFile(t, auto, "shared.md", memoryDoc("a", "auto 来源"), "body", 0)
	writeMemoryFile(t, extra1, "shared.md", memoryDoc("b", "extra 来源一"), "body", 0)
	writeMemoryFile(t, extra2, "shared.md", memoryDoc("c", "extra 来源二"), "body", 0)

	store := &memoryStore{autoDir: auto, extraDirs: []string{extra1, extra2}}
	headers := store.scan("")
	if len(headers) != 3 {
		t.Fatalf("scanned %d headers, want 3: %+v", len(headers), headers)
	}
	wantNames := map[string]bool{"shared.md": false, "extra-0/shared.md": false, "extra-1/shared.md": false}
	for _, h := range headers {
		wantNames[h.Filename] = true
	}
	for name, seen := range wantNames {
		if !seen {
			t.Fatalf("expected filename %q in scan result", name)
		}
	}
	for _, h := range headers {
		if strings.HasPrefix(h.Filename, "extra-") && h.Source != memorySourceExtra {
			t.Fatalf("extra file %q must carry extra source, got %q", h.Filename, h.Source)
		}
	}
}

func TestMemoryStoreScanMissingDirIsNoop(t *testing.T) {
	store := &memoryStore{autoDir: filepath.Join(t.TempDir(), "not-exist")}
	if headers := store.scan(""); len(headers) != 0 {
		t.Fatalf("missing dir must scan to empty, got %+v", headers)
	}
}

func TestBuildMemoryManifestLineFormat(t *testing.T) {
	dir := t.TempDir()
	writeMemoryFile(t, dir, "user_role.md", memoryDoc("user role", "用户角色", "user"), "body", 0)
	store := &memoryStore{autoDir: dir}
	headers := store.scan("")
	if len(headers) != 1 {
		t.Fatalf("scanned %d headers, want 1", len(headers))
	}
	manifest := buildMemoryManifest(headers)
	if got := strings.Count(manifest, "\n") + 1; got != 1 {
		t.Fatalf("manifest should be one line, got %d", got)
	}
	// ISO8601 秒级 UTC Z 时间戳。
	stamp := headers[0].ModTime.UTC().Format("2006-01-02T15:04:05Z")
	want := fmt.Sprintf("- [user] user_role.md (%s): 用户角色", stamp)
	if manifest != want {
		t.Fatalf("manifest line = %q, want %q", manifest, want)
	}
	if !strings.HasSuffix(stamp, "Z") || strings.Contains(stamp, "+") {
		t.Fatalf("timestamp %q must be second-precision UTC Z", stamp)
	}
}

func TestSurfaceMemoryDocumentTruncation(t *testing.T) {
	t.Run("no truncation", func(t *testing.T) {
		dir := t.TempDir()
		path := writeMemoryFile(t, dir, "small.md", memoryDoc("s", "小文件"), strings.Repeat("x", 100), 0)
		got, truncated, err := surfaceMemoryDocument(path, filepath.ToSlash(path))
		if err != nil || truncated {
			t.Fatalf("small file should surface intact, truncated=%v err=%v", truncated, err)
		}
		if !strings.Contains(got, strings.Repeat("x", 100)) {
			t.Fatal("content missing")
		}
	})

	t.Run("line truncation appends hint", func(t *testing.T) {
		dir := t.TempDir()
		path := writeMemoryFile(t, dir, "many_lines.md", memoryDoc("m", "多行"),
			strings.Repeat("line\n", memoryMaxLines+50), 0)
		got, truncated, err := surfaceMemoryDocument(path, filepath.ToSlash(path))
		if err != nil || !truncated {
			t.Fatalf("over-200-lines must truncate, truncated=%v err=%v", truncated, err)
		}
		wantHint := fmt.Sprintf("> This memory file was truncated. Read the complete file at: %s", filepath.ToSlash(path))
		if !strings.HasSuffix(got, wantHint) {
			t.Fatalf("truncation hint missing/malformed, got suffix %q", got[max(0, len(got)-200):])
		}
		if gotLines := strings.Count(got, "\n"); gotLines > memoryMaxLines+2 {
			t.Fatalf("surfaced %d lines, want <= %d (+hint)", gotLines, memoryMaxLines)
		}
	})

	t.Run("byte truncation keeps valid utf8", func(t *testing.T) {
		dir := t.TempDir()
		// 中文字符每字 3 字节；4000 个中文 = 12000 字节，必然触发字节截断，
		// 且截断点落在多字节字符中间时必须回退到 rune 边界。
		path := writeMemoryFile(t, dir, "wide.md", memoryDoc("w", "宽字符"), strings.Repeat("记忆内容", 1000), 0)
		got, truncated, err := surfaceMemoryDocument(path, filepath.ToSlash(path))
		if err != nil || !truncated {
			t.Fatalf("over-4096-bytes must truncate, truncated=%v err=%v", truncated, err)
		}
		hint := fmt.Sprintf("> This memory file was truncated. Read the complete file at: %s", filepath.ToSlash(path))
		body := strings.TrimSuffix(got, "\n\n"+hint)
		if len(body) > memoryMaxSurfacedBytes {
			t.Fatalf("surfaced body %d bytes > cap %d", len(body), memoryMaxSurfacedBytes)
		}
		if !utf8.ValidString(body) {
			t.Fatal("byte truncation must not split multi-byte runes")
		}
	})
}

// TestMemoryWriteNoteSessionScoped 验证防双写记账的会话桶收敛（PerSession）：
// 只有落进当前会话桶的写入才置位防双写标志——写错位置（如桶根，PerSession
// 召回扫不到）时不能压制抽取，否则由抽取子代理兜底写入会话桶的路径被堵死。
func TestMemoryWriteNoteSessionScoped(t *testing.T) {
	dir := t.TempDir()
	store := &memoryStore{autoDir: dir, perSession: true}
	m := &memoryRuntime{
		cfg:   MemoryOptions{Dir: dir, PerSession: true},
		store: store,
		guard: &memoryWriteGuard{autoDir: dir},
	}
	bucket := store.sessionBucketDir("dingtalk:user:1001")

	// 写进会话桶：置位（防双写按设计跳过本轮抽取）。
	m.noteSuccessfulMemoryWrite("write", map[string]any{
		"file_path": filepath.Join(bucket, "user_role.md"),
		"content":   "x",
	}, "dingtalk:user:1001")
	if !m.writeObserved.Load() {
		t.Fatal("write into the session bucket must set writeObserved")
	}

	// 写进桶根（PerSession 召回扫不到的搁浅位置）：不置位，抽取兜底仍会跑。
	m.writeObserved.Store(false)
	m.noteSuccessfulMemoryWrite("write", map[string]any{
		"file_path": filepath.Join(dir, "stranded.md"),
		"content":   "x",
	}, "dingtalk:user:1001")
	if m.writeObserved.Load() {
		t.Fatal("write outside the session bucket must not set writeObserved in PerSession mode")
	}

	// 全局桶模式：桶根写入照旧置位（历史行为不变）。
	global := &memoryRuntime{
		cfg:   MemoryOptions{Dir: dir},
		store: &memoryStore{autoDir: dir},
		guard: &memoryWriteGuard{autoDir: dir},
	}
	global.noteSuccessfulMemoryWrite("write", map[string]any{
		"file_path": filepath.Join(dir, "legacy.md"),
		"content":   "x",
	}, "dingtalk:user:1001")
	if !global.writeObserved.Load() {
		t.Fatal("global mode root write must still set writeObserved")
	}
}

// TestMemoryStorePerSessionIsolation 验证 PerSession 会话级记忆隔离的核心语义：
// 各会话只看到自己桶内的记忆（多用户 IM 场景防串扰），extraDirs 仍跨会话共享，
// 全局桶模式（perSession=false）下行为与历史一致。
func TestMemoryStorePerSessionIsolation(t *testing.T) {
	dir := t.TempDir()
	store := &memoryStore{autoDir: dir, perSession: true}

	// 会话桶目录名与 sessionID 由 sessionBucketDir 派生：写入前先取派生名。
	bucketA := store.sessionBucketDir("dingtalk:user:1001")
	bucketB := store.sessionBucketDir("dingtalk:user:2002")
	if bucketA == bucketB {
		t.Fatalf("distinct sessions must map to distinct buckets, both %q", bucketA)
	}
	if !strings.Contains(filepath.Base(bucketA), "dingtalk-user-1001") {
		t.Fatalf("bucket name must sanitize sessionID, got %q", filepath.Base(bucketA))
	}
	writeMemoryFile(t, bucketA, "user_role.md", memoryDoc("a", "用户甲偏好"), "A 的记忆", 0)
	writeMemoryFile(t, bucketB, "user_role.md", memoryDoc("b", "用户乙偏好"), "B 的记忆", 0)

	// 会话 A 只召回 A 的记忆，看不到 B 的。
	headersA := store.scan("dingtalk:user:1001")
	if len(headersA) != 1 || headersA[0].Path != filepath.Join(bucketA, "user_role.md") {
		t.Fatalf("session A scan = %+v, want only its own bucket file", headersA)
	}
	headersB := store.scan("dingtalk:user:2002")
	if len(headersB) != 1 || headersB[0].Path != filepath.Join(bucketB, "user_role.md") {
		t.Fatalf("session B scan = %+v, want only its own bucket file", headersB)
	}

	// extraDirs 跨会话共享：任一会话都能召回。
	extra := t.TempDir()
	writeMemoryFile(t, extra, "shared.md", memoryDoc("s", "共享知识"), "共享内容", 0)
	store.extraDirs = []string{extra}
	for _, sid := range []string{"dingtalk:user:1001", "dingtalk:user:2002"} {
		found := false
		for _, h := range store.scan(sid) {
			if h.Source == memorySourceExtra {
				found = true
			}
		}
		if !found {
			t.Fatalf("session %s must still see extra dirs (shared recall source)", sid)
		}
	}

	// 全局桶模式（perSession=false）：忽略会话 ID，扫 autoDir 本身（历史行为；
	// WalkDir 递归连子目录一起扫，故用独立干净目录断言）。
	globalDir := t.TempDir()
	writeMemoryFile(t, globalDir, "legacy.md", memoryDoc("l", "旧桶根记忆"), "桶根内容", 0)
	global := &memoryStore{autoDir: globalDir}
	if got := len(global.scan("dingtalk:user:1001")); got != 1 {
		t.Fatalf("global mode scan = %d headers, want 1 from autoDir root", got)
	}

	// 游标按会话隔离：extractor 的 statePathFor 落在各自桶内。
	rt := &Runtime{memory: &memoryRuntime{store: store}}
	ext := &memoryExtractor{rt: rt}
	pathA := ext.statePathFor("dingtalk:user:1001")
	if !strings.HasPrefix(pathA, bucketA) {
		t.Fatalf("state file for session A must live in its bucket: %q", pathA)
	}
	if filepath.Base(pathA) != ".memory_state.json" {
		t.Fatalf("state file name = %q", filepath.Base(pathA))
	}
}
