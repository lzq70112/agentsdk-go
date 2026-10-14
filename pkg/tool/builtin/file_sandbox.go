package toolbuiltin

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lzq70112/agentsdk-go/pkg/sandbox"
)

const defaultMaxFileBytes = 1 << 20 // 1 MiB

// fileSandbox enforces sandboxed filesystem operations shared by file tools.
type fileSandbox struct {
	policy   sandbox.FileSystemPolicy
	root     string
	maxBytes int64
}

func newFileSandbox(root string) *fileSandbox {
	resolved := resolveRoot(root)
	return newFileSandboxWithSandbox(resolved, sandbox.NewFileSystemAllowList(resolved))
}

func newFileSandboxWithSandbox(root string, policy sandbox.FileSystemPolicy) *fileSandbox {
	return &fileSandbox{
		policy:   policy,
		root:     resolveRoot(root),
		maxBytes: defaultMaxFileBytes,
	}
}

func (f *fileSandbox) resolvePath(raw interface{}) (string, error) {
	if f == nil {
		return "", errors.New("file sandbox is not initialised")
	}
	if raw == nil {
		return "", errors.New("path is required")
	}
	pathStr, err := coerceString(raw)
	if err != nil {
		return "", fmt.Errorf("path must be string: %w", err)
	}
	trimmed := strings.TrimSpace(pathStr)
	if trimmed == "" {
		return "", errors.New("path cannot be empty")
	}
	// Windows 下 / 开头的 POSIX 风格绝对路径无法定位（filepath.IsAbs 返回
	// false），会被误当相对路径拼进沙箱根（如 /tmp/x → <root>\tmp\x），静默
	// 在项目内创建目录污染工作区。显式拒绝并引导改用相对路径；// 开头的
	// UNC 路径与 Linux/macOS 的原生绝对路径不受影响。
	if runtime.GOOS == "windows" && strings.HasPrefix(trimmed, "/") && !strings.HasPrefix(trimmed, "//") {
		return "", fmt.Errorf(
			"POSIX-style absolute path %q cannot be resolved on Windows; use a path relative to the sandbox root (e.g. %q) or a Windows absolute path (C:\\...)",
			trimmed, strings.TrimPrefix(trimmed, "/"))
	}
	candidate := trimmed
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(f.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	if f.policy != nil {
		if err := f.policy.Validate(candidate); err != nil {
			return "", err
		}
	}
	return candidate, nil
}

func (f *fileSandbox) readFile(path string) (string, error) {
	if f == nil {
		return "", errors.New("file sandbox is not initialised")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	if f.maxBytes > 0 && info.Size() > f.maxBytes {
		return "", fmt.Errorf("file exceeds %d bytes limit", f.maxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("binary file %s is not supported", path)
	}
	return string(data), nil
}

func (f *fileSandbox) writeFile(path string, content string) error {
	if f == nil {
		return errors.New("file sandbox is not initialised")
	}
	data := []byte(content)
	if f.maxBytes > 0 && int64(len(data)) > f.maxBytes {
		return fmt.Errorf("content exceeds %d bytes limit", f.maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("ensure directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o666); err != nil { //nolint:gosec // respect umask for created files
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}
