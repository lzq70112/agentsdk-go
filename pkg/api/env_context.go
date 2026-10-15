package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// envProbeTimeout 是单个环境探测命令的上限。环境探测是尽力而为的：任何
// 一步失败都静默省略对应行，绝不允许环境采集阻塞或失败 runtime 初始化。
const envProbeTimeout = 1500 * time.Millisecond

// buildEnvironmentSection 采集运行环境上下文，对齐 Claude Code 的
// # Context / <env> + <git_env>：模型不知道自己 cwd、平台、git 状态时只能
// 靠猜，文件路径与 shell 语义随之打偏。
func buildEnvironmentSection(projectRoot, sandboxRoot string) string {
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	b.WriteString("You have been invoked in the following environment:\n\n")
	b.WriteString(fmt.Sprintf("- Working directory: %s\n", projectRoot))
	if sandboxRoot != "" && sandboxRoot != projectRoot {
		b.WriteString(fmt.Sprintf("- Sandbox root: %s\n", sandboxRoot))
	}
	writeGitInfo(&b, projectRoot)
	b.WriteString(fmt.Sprintf("- Platform: %s/%s\n", runtime.GOOS, runtime.GOARCH))
	if osVer := probeOSVersion(); osVer != "" {
		b.WriteString(fmt.Sprintf("- OS Version: %s\n", osVer))
	}
	if shell := probeShell(); shell != "" {
		b.WriteString(fmt.Sprintf("- Shell: %s\n", shell))
	}
	b.WriteString(fmt.Sprintf("- Current date: %s\n", time.Now().Format("2006-01-02")))
	return b.String()
}

// writeGitInfo 报告 git 仓库状态。非 git 目录零 exec：.git 存在性检测先行。
func writeGitInfo(b *strings.Builder, projectRoot string) {
	if _, err := os.Stat(filepath.Join(projectRoot, ".git")); err != nil {
		b.WriteString("- Is a git repository: false\n")
		return
	}
	b.WriteString("- Is a git repository: true\n")
	if out := runProbe(projectRoot, "git", "rev-parse", "--abbrev-ref", "origin/HEAD"); out != "" {
		b.WriteString(fmt.Sprintf("- Main branch: %s\n", out))
	} else if out := runProbe(projectRoot, "git", "rev-parse", "--abbrev-ref", "HEAD"); out != "" {
		b.WriteString(fmt.Sprintf("- Main branch: %s\n", out))
	}
	if out := runProbe(projectRoot, "git", "remote", "get-url", "origin"); out != "" {
		b.WriteString(fmt.Sprintf("- Git remote: %s\n", out))
	}
}

// probeOSVersion 尽力而为取 OS 版本：Windows 用 cmd /c ver，其余 uname -sr。
func probeOSVersion() string {
	if runtime.GOOS == "windows" {
		out := runProbe("", "cmd", "/c", "ver")
		if i := strings.Index(out, "[Version "); i >= 0 {
			if j := strings.Index(out[i:], "]"); j > 0 {
				return "Windows (" + out[i+len("[Version "):i+j] + ")"
			}
		}
		return ""
	}
	return runProbe("", "uname", "-sr")
}

// probeShell 报告实际 shell：优先 SHELL（Git Bash/Unix），Windows 回退 ComSpec。
func probeShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return filepath.Base(s)
	}
	if s := os.Getenv("ComSpec"); s != "" {
		return filepath.Base(s)
	}
	return ""
}

// runProbe 在 dir 下执行探测命令，超时或失败返回空串。
func runProbe(dir, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), envProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
