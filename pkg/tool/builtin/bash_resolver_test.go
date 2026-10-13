package toolbuiltin

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// resetBashResolver 把解析器全局状态与测试接缝换成可注入的桩，并在测试
// 结束后还原为真实实现，避免污染同包其他测试。
func resetBashResolver(t *testing.T) {
	t.Helper()
	savedOnce, savedResolution := bashResolveOnce, bashResolution
	savedGOOS, savedEnv := bashResolverGOOS, bashResolverEnv
	savedLookPath, savedProbe := bashResolverLookPath, bashResolverRunProbe
	savedGetwd := bashGetwd
	t.Cleanup(func() {
		bashResolveOnce, bashResolution = savedOnce, savedResolution
		bashResolverGOOS, bashResolverEnv = savedGOOS, savedEnv
		bashResolverLookPath, bashResolverRunProbe = savedLookPath, savedProbe
		bashGetwd = savedGetwd
	})
	bashResolveOnce = new(sync.Once)
	bashResolution = BashProviderDiagnostic{}
	bashResolverGOOS = func() string { return runtime.GOOS }
	bashResolverEnv = os.Getenv
	bashResolverLookPath = exec.LookPath
	bashResolverRunProbe = runBashProbe
	bashGetwd = os.Getwd
}

// TestBashResolverEnvOverrideWinsWhenProbePasses 验证部署方显式指定的
// bash 优先级最高：探活通过即采纳，不再尝试后续候选。
func TestBashResolverEnvOverrideWinsWhenProbePasses(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "windows" }
	bashResolverEnv = func(key string) string {
		if key == "AGENTSDK_BASH_EXECUTABLE" {
			return `D:\tools\my-bash.exe`
		}
		return ""
	}
	bashResolverRunProbe = func(path string) (string, error) {
		if path == `D:\tools\my-bash.exe` {
			return "probe-ok", nil
		}
		return "", errors.New("unexpected probe target: " + path)
	}

	path, diag := ResolveBashExecutable()
	if path != `D:\tools\my-bash.exe` || diag.Source != "env" || !diag.ProbeVerified {
		t.Fatalf("diag = %+v, want env override adopted with probe verified", diag)
	}
}

// TestBashResolverWindowsNeverPathSearchBash 固化 Windows 铁律：解析器
// 永远不得对 PATH 搜索 bash/bash.exe——system32\bash.exe 是 WSL stub，
// 命中它会让 Bash 工具静默瘫痪；应从 Git 安装位置反推。
func TestBashResolverWindowsNeverPathSearchBash(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "windows" }
	bashResolverEnv = func(string) string { return "" }
	var lookedUp []string
	bashResolverLookPath = func(name string) (string, error) {
		lookedUp = append(lookedUp, name)
		if name == "git.exe" {
			return `D:\Tools\Git\cmd\git.exe`, nil
		}
		return "", errors.New("not found")
	}
	bashResolverRunProbe = func(path string) (string, error) {
		if path == `D:\Tools\Git\bin\bash.exe` {
			return "probe-ok", nil
		}
		return "", errors.New("no such file")
	}

	path, diag := ResolveBashExecutable()
	if path != `D:\Tools\Git\bin\bash.exe` || diag.Source != "windows-git" || !diag.ProbeVerified {
		t.Fatalf("diag = %+v, want git-derived bash adopted", diag)
	}
	for _, name := range lookedUp {
		if name == "bash" || name == "bash.exe" {
			t.Fatalf("Windows 上解析器对 PATH 搜索了 %q，违反铁律（lookedUp=%v）", name, lookedUp)
		}
	}
}

// TestBashResolverWindowsGitUnderCwdIgnored 验证 cwd 投放防护：当前工作
// 目录下的 git.exe（如恶意仓库携带）不得用于推导 bash 位置。
func TestBashResolverWindowsGitUnderCwdIgnored(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "windows" }
	wd := t.TempDir()
	bashGetwd = func() (string, error) { return wd, nil }
	bashResolverEnv = func(string) string { return "" }
	bashResolverLookPath = func(name string) (string, error) {
		if name == "git.exe" {
			return filepath.Join(wd, "git.exe"), nil
		}
		return "", errors.New("not found")
	}
	bashResolverRunProbe = func(path string) (string, error) {
		if strings.HasPrefix(path, wd) {
			t.Errorf("cwd 下投放的 git.exe 推导路径被探测: %s", path)
		}
		return "", errors.New("probe fail")
	}

	_, diag := ResolveBashExecutable()
	if diag.Source != "fallback" {
		t.Fatalf("diag = %+v, want fallback (git under cwd must be ignored)", diag)
	}
}

// TestBashResolverWindowsMingw64GitLayout 覆盖 git.exe 位于 mingw64\bin
// 的安装布局（git 装在自定义盘符时常见）：反推需向上走到 Git 根的
// bin\bash.exe，而不是停在 mingw64\bin 下找不到就放弃。
func TestBashResolverWindowsMingw64GitLayout(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "windows" }
	bashResolverEnv = func(string) string { return "" }
	bashResolverLookPath = func(name string) (string, error) {
		if name == "git.exe" {
			return `E:\app\Git\mingw64\bin\git.exe`, nil
		}
		return "", errors.New("not found")
	}
	bashResolverRunProbe = func(path string) (string, error) {
		if path == `E:\app\Git\bin\bash.exe` {
			return "probe-ok", nil
		}
		return "", errors.New("no such file")
	}

	path, diag := ResolveBashExecutable()
	if path != `E:\app\Git\bin\bash.exe` || diag.Source != "windows-git" || !diag.ProbeVerified {
		t.Fatalf("diag = %+v, want Git root bin\\bash.exe via ancestor walk", diag)
	}
}

// TestBashResolverFirstProbePassWins 验证阶梯语义：按优先级逐个探活，
// 第一个通过者胜出；更早候选的失败保留在 LastFailure 供诊断。
func TestBashResolverFirstProbePassWins(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "windows" }
	bashResolverEnv = func(key string) string {
		if key == "AGENTSDK_BASH_EXECUTABLE" {
			return `D:\broken\bash.exe`
		}
		return ""
	}
	bashResolverLookPath = func(string) (string, error) { return "", errors.New("not found") }
	bashResolverRunProbe = func(path string) (string, error) {
		if path == `C:\Program Files\Git\bin\bash.exe` {
			return "probe-ok", nil
		}
		return "", errors.New("no such file")
	}

	_, diag := ResolveBashExecutable()
	if diag.Source != "windows-constant" || diag.Path != `C:\Program Files\Git\bin\bash.exe` || !diag.ProbeVerified {
		t.Fatalf("diag = %+v, want first passing candidate adopted", diag)
	}
	if diag.LastFailure == "" || !strings.Contains(diag.LastFailure, "env") {
		t.Fatalf("LastFailure = %q, want earlier env candidate failure recorded", diag.LastFailure)
	}
}

// TestBashResolverAllProbesFailFallsBackUnverified 验证兜底语义：所有
// 候选探活失败（含超时）时，退回字面量 bash 并标记 probe_verified=false，
// 不让 Bash 工具直接瘫痪。
func TestBashResolverAllProbesFailFallsBackUnverified(t *testing.T) {
	resetBashResolver(t)
	bashResolverGOOS = func() string { return "linux" }
	bashResolverEnv = func(string) string { return "" }
	bashResolverLookPath = func(string) (string, error) { return "", errors.New("not found") }
	bashResolverRunProbe = func(string) (string, error) {
		return "", errors.New("context deadline exceeded")
	}

	_, diag := ResolveBashExecutable()
	if diag.Path != "bash" || diag.Source != "fallback" || diag.ProbeVerified {
		t.Fatalf("diag = %+v, want literal bash fallback with probe_verified=false", diag)
	}
	if diag.LastFailure == "" {
		t.Fatalf("LastFailure = empty, want probe failure recorded")
	}
}

// TestBashResolverResolutionCachedAcrossCalls 验证进程级缓存：第二次
// 调用不重新枚举、不重新探活（主/子 runtime 共享，探测只发生一次）。
func TestBashResolverResolutionCachedAcrossCalls(t *testing.T) {
	resetBashResolver(t)
	calls := 0
	bashResolverGOOS = func() string { return "linux" }
	bashResolverLookPath = func(name string) (string, error) {
		if name == "bash" {
			calls++
			return "/usr/bin/bash", nil
		}
		return "", errors.New("not found")
	}
	bashResolverRunProbe = func(string) (string, error) { return "ok", nil }

	p1, diag1 := ResolveBashExecutable()
	p2, diag2 := ResolveBashExecutable()
	if p1 != p2 || p1 != "/usr/bin/bash" || diag1.Source != "path" {
		t.Fatalf("p1=%q p2=%q diag1=%+v", p1, p2, diag1)
	}
	if diag1 != diag2 {
		t.Fatalf("diagnostics differ: %+v vs %+v", diag1, diag2)
	}
	if calls != 1 {
		t.Fatalf("LookPath(bash) called %d times, want 1 (cached)", calls)
	}
}

// TestBashProbeScriptCoversMultiLineAndNonASCII 固化探活 payload 的设计
// 意图：多行暴露传输层换行 mangle，非 ASCII 暴露编码 mangle——探活不
// 只是"能启动"，而是"传输层与真实执行一致"。
func TestBashProbeScriptCoversMultiLineAndNonASCII(t *testing.T) {
	script := bashProbeScript()
	if !strings.Contains(script, "\n") {
		t.Fatal("探活 payload 必须含多行，暴露传输层换行 mangle")
	}
	if !strings.Contains(script, "探活") || !strings.Contains(script, "πλü") {
		t.Fatal("探活 payload 必须含非 ASCII 字符，暴露编码 mangle")
	}
}

// TestRunBashProbeRealBash 用本机真实 bash 验证探活通道端到端有效
// （探活通过 = 真实执行通道健康）。
func TestRunBashProbeRealBash(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("本机无 bash，跳过真实探活")
	}
	out, err := runBashProbe(bashPath)
	if err != nil {
		t.Fatalf("runBashProbe(%s) failed: %v, output=%q", bashPath, err, out)
	}
}
