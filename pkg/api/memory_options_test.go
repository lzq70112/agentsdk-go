package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryOptionsWithDefaults(t *testing.T) {
	root := t.TempDir()
	opts := Options{ProjectRoot: root}.withDefaults()
	if opts.Memory == nil {
		t.Fatal("memory options must always be defaulted (enabled by default)")
	}
	if !opts.Memory.enabled() {
		t.Fatal("memory should be enabled by default")
	}
	if opts.Memory.SelectorTier != ModelTierLow {
		t.Fatalf("selector tier default = %q, want %q", opts.Memory.SelectorTier, ModelTierLow)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	home, ok := memoryHomeDir()
	if !ok {
		t.Fatal("memory home dir must resolve in tests")
	}
	wantDir := filepath.Join(home, ".agents", "memory", memoryBucketName(absRoot))
	if opts.Memory.Dir != wantDir {
		t.Fatalf("memory dir = %q, want %q", opts.Memory.Dir, wantDir)
	}
	if !filepath.IsAbs(opts.Memory.Dir) {
		t.Fatalf("memory dir %q should be absolute", opts.Memory.Dir)
	}
}

func TestMemoryBucketName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"windows path", `E:\work\我的项目`, "e--work-我的项目"},
		{"posix path", "/home/user/repo", "-home-user-repo"},
		{"mixed", `D:/a\b:c`, "d--a-b-c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memoryBucketName(tt.in); got != tt.want {
				t.Fatalf("memoryBucketName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMemoryOptionsDisabledByWithMemoryDisabled(t *testing.T) {
	root := t.TempDir()
	opts := Options{ProjectRoot: root, Memory: &MemoryOptions{Dir: t.TempDir()}}
	WithMemoryDisabled()(&opts)
	opts = opts.withDefaults()
	if opts.Memory.enabled() {
		t.Fatal("WithMemoryDisabled must disable memory")
	}
	if opts.Memory.Dir == "" {
		t.Fatal("explicit Dir must be preserved even when disabled")
	}
}

func TestMemoryOptionsExplicitDirOverridesDerived(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	opts := Options{
		ProjectRoot: root,
		Memory:      &MemoryOptions{Dir: dir},
	}.withDefaults()
	if !opts.Memory.enabled() {
		t.Fatal("memory with explicit dir should stay enabled")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs dir: %v", err)
	}
	if opts.Memory.Dir != absDir {
		t.Fatalf("memory dir = %q, want explicit %q", opts.Memory.Dir, absDir)
	}
}

func TestMemoryOptionsUnresolvableProjectRootDisablesMemory(t *testing.T) {
	// ProjectRoot 真正为空（无桶名可派生）时 memory 必须禁用；该分支只能通过
	// 直接调用 MemoryOptions.withDefaults 触达（Options.withDefaults 会先把
	// ProjectRoot 兜底为"."并回退 cwd 桶）。
	cfg := (&MemoryOptions{}).withDefaults("")
	if cfg.enabled() {
		t.Fatalf("memory must be disabled when project root is empty, got %+v", cfg)
	}
}

func TestMemoryOptionsFrozenClonesExtraDirs(t *testing.T) {
	opts := Options{Memory: &MemoryOptions{ExtraDirs: []string{"/a"}}}
	frozen := opts.frozen()
	opts.Memory.ExtraDirs[0] = "/changed"
	if frozen.Memory == nil {
		t.Fatal("frozen must keep memory options")
	}
	if len(frozen.Memory.ExtraDirs) != 1 || frozen.Memory.ExtraDirs[0] != "/a" {
		t.Fatalf("frozen ExtraDirs = %v, want [/a]", frozen.Memory.ExtraDirs)
	}
}

func TestMemoryOptionsWithMemoryOptionsSetter(t *testing.T) {
	dir := t.TempDir()
	opts := Options{}
	WithMemoryOptions(MemoryOptions{Dir: dir})(&opts)
	if opts.Memory == nil || opts.Memory.Dir != dir {
		t.Fatalf("WithMemoryOptions should set memory dir, got %+v", opts.Memory)
	}
	if !opts.Memory.enabled() {
		t.Fatal("WithMemoryOptions with zero Enabled must default to enabled")
	}
}

func TestMemoryEnabledWithoutUserHome(t *testing.T) {
	// HOME 指向临时目录时默认派生仍可用（memoryHomeDir 走 HOME 优先，
	// 与 settings_loader 的解析顺序一致，保证测试隔离生效）。
	t.Setenv("HOME", t.TempDir())
	opts := Options{ProjectRoot: t.TempDir()}.withDefaults()
	if !opts.Memory.enabled() || opts.Memory.Dir == "" {
		t.Fatalf("memory should derive default dir, got %+v", opts.Memory)
	}
	if home := os.Getenv("HOME"); !strings.HasPrefix(opts.Memory.Dir, home) {
		t.Fatalf("derived dir %q should live under HOME %q", opts.Memory.Dir, home)
	}
}
