package toolbuiltin

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// bash 解析三段式：候选枚举 → 探活验证 → 进程级缓存。核心原则：PATH 命中
// ≠ 可用，候选必须真跑一次探活才被采信；探活超时候选只做未验证兜底，
// 验证过的永远优先。
//
// Windows 铁律：禁止 PATH 搜索 bash。C:\Windows\system32\bash.exe 是 WSL
// 启动器 stub（未装发行版时全部命令 exit 1），PATH 命中它会让整个 Bash
// 工具静默瘫痪；改为从 Git for Windows 安装位置反推 bash。

const (
	// bashProbeTimeout 限制单个候选的探活耗时。
	bashProbeTimeout = 3 * time.Second

	// bashEnvOverride 允许部署方显式指定 bash 路径，优先级最高。
	bashEnvOverride = "AGENTSDK_BASH_EXECUTABLE"
)

// bashProbeScript 是探活 payload：刻意包含多行与非 ASCII 字符，让传输层
// 的换行/编码 mangle 在探活期暴露，而不是在第一条真实命令时才暴露。
func bashProbeScript() string {
	return "echo 'agentsdk-bash-probe-探活-πλü-1'\necho 'agentsdk-probe-line-2'\nexit 0\n"
}

// BashProviderDiagnostic 是 bash 解析结果的只读描述，供宿主程序（如
// admin 页面）展示。Source 取值：env / windows-constant /
// windows-localappdata / windows-git / path / fallback。
type BashProviderDiagnostic struct {
	Source        string
	Path          string
	ProbeVerified bool
	ProbeCommand  string
	LastFailure   string
}

// 测试接缝（与 bash.go 的 bashGetwd 等接缝同风格）。
var (
	bashResolverGOOS     = func() string { return runtime.GOOS }
	bashResolverEnv      = os.Getenv
	bashResolverLookPath = exec.LookPath
	bashResolverRunProbe = runBashProbe
)

var (
	bashResolveOnce = new(sync.Once)
	bashResolution  BashProviderDiagnostic
)

// ResolveBashExecutable 返回 Bash 工具执行所用的 bash 二进制及其诊断信息。
// 每个进程只解析一次，主/子 runtime 共享结果，不重复探测。
func ResolveBashExecutable() (string, BashProviderDiagnostic) {
	bashResolveOnce.Do(func() {
		bashResolution = resolveBashExecutable()
		rgState := "not found"
		if rg, err := bashResolverLookPath("rg"); err == nil {
			rgState = rg
		}
		log.Printf("agentsdk: bash resolver: source=%s path=%s probe_verified=%v last_failure=%q rg=%s",
			bashResolution.Source, bashResolution.Path, bashResolution.ProbeVerified,
			bashResolution.LastFailure, rgState)
	})
	return bashResolution.Path, bashResolution
}

// BashResolutionDiagnostic 返回缓存的解析诊断，首次调用会触发解析。
func BashResolutionDiagnostic() BashProviderDiagnostic {
	_, diag := ResolveBashExecutable()
	return diag
}

type bashCandidate struct {
	path   string
	source string
}

func resolveBashExecutable() BashProviderDiagnostic {
	candidates := bashCandidateLadder()
	// 字面量兜底恒在梯队末位：全部候选探活失败时退回它（未验证兜底），
	// 行为不劣于解析器引入前的"裸 bash"。
	literal := candidates[len(candidates)-1]
	var lastFailure string
	for _, c := range candidates {
		_, err := bashResolverRunProbe(c.path)
		if err == nil {
			return BashProviderDiagnostic{
				Source:        c.source,
				Path:          c.path,
				ProbeVerified: true,
				ProbeCommand:  probeCommandDescription(c.path),
				LastFailure:   lastFailure,
			}
		}
		lastFailure = fmt.Sprintf("%s: %s", c.source, probeFailureSummary(err))
	}
	return BashProviderDiagnostic{
		Source:        literal.source,
		Path:          literal.path,
		ProbeVerified: false,
		ProbeCommand:  probeCommandDescription(literal.path),
		LastFailure:   lastFailure,
	}
}

// bashCandidateLadder 按优先级枚举候选：env 覆盖 → Windows 从 Git 反推
// （铁律：不走 PATH）→ 非 Windows LookPath → 字面量兜底。
func bashCandidateLadder() []bashCandidate {
	var out []bashCandidate
	if p := strings.TrimSpace(bashResolverEnv(bashEnvOverride)); p != "" {
		out = append(out, bashCandidate{p, "env"})
	}
	if bashResolverGOOS() == "windows" {
		out = append(out, windowsBashCandidates()...)
	} else if p, err := bashResolverLookPath("bash"); err == nil {
		out = append(out, bashCandidate{p, "path"})
	}
	out = append(out, bashCandidate{"bash", "fallback"})
	return out
}

// windowsBashCandidates 枚举 Windows 上的 bash 候选，绝不包含 PATH 搜索。
func windowsBashCandidates() []bashCandidate {
	var out []bashCandidate
	for _, p := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
	} {
		out = append(out, bashCandidate{p, "windows-constant"})
	}
	if local := bashResolverEnv("LOCALAPPDATA"); local != "" {
		out = append(out, bashCandidate{
			filepath.Join(local, "Programs", "Git", "bin", "bash.exe"),
			"windows-localappdata",
		})
	}
	if gitExe, err := bashResolverLookPath("git.exe"); err == nil && !isPathUnderCwd(gitExe) {
		out = append(out, bashCandidatesFromGit(gitExe)...)
	}
	return out
}

// bashCandidatesFromGit 从 git.exe 位置反推 bash，覆盖两种已知布局：
//
//	<x>\cmd\git.exe → <x>\bin\bash.exe（Git for Windows 标准布局）
//	<x>\bin\git.exe → <x>\bash.exe（usr\bin 布局）
//
// 其他布局（如 <x>\mingw64\bin\git.exe）向上最多走 3 层，逐层尝试
// <层>\bin\bash.exe 与 <层>\bash.exe——Git 根下的 bin\bash.exe 是官方
// 包装脚本，usr\bin\bash.exe 是真实二进制，两者皆可用。
func bashCandidatesFromGit(gitExe string) []bashCandidate {
	dir := filepath.Dir(gitExe)
	var out []bashCandidate
	add := func(p string) {
		out = append(out, bashCandidate{p, "windows-git"})
	}
	switch strings.ToLower(filepath.Base(dir)) {
	case "cmd":
		add(filepath.Join(dir, "..", "bin", "bash.exe"))
	case "bin":
		add(filepath.Join(dir, "bash.exe"))
	}
	for i := 0; i < 3 && dir != filepath.Dir(dir); i++ {
		add(filepath.Join(dir, "bin", "bash.exe"))
		add(filepath.Join(dir, "bash.exe"))
		dir = filepath.Dir(dir)
	}
	return out
}

// isPathUnderCwd 报告 path 是否位于当前工作目录内。cwd 下被投放的
// git.exe（如恶意仓库携带）不得用于推导 bash 位置。
func isPathUnderCwd(path string) bool {
	wd, err := bashGetwd()
	if err != nil {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absWd, err := filepath.Abs(wd)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absWd, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runBashProbe 对候选 bash 真跑一次探活命令：验证它不仅能启动，还能正确
// 执行含多行与非 ASCII 的脚本。超时 3 秒。
func runBashProbe(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bashProbeTimeout)
	defer cancel()
	script := bashProbeScript()
	out, err := exec.CommandContext(ctx, path, "-c", script).CombinedOutput()
	if err != nil {
		return string(out), err
	}
	if !strings.Contains(string(out), "agentsdk-bash-probe-探活-πλü-1") ||
		!strings.Contains(string(out), "agentsdk-probe-line-2") {
		return string(out), fmt.Errorf("probe output missing expected markers")
	}
	return string(out), nil
}

func probeCommandDescription(path string) string {
	return fmt.Sprintf("%s -c %q", path, bashProbeScript())
}

func probeFailureSummary(err error) string {
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}
