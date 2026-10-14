package toolbuiltin

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Bash 非零退出码语义（对齐 Claude Code，全量统一）：
//
// POSIX 里 exit code != 0 表示「有话要说」，不表示「出错了」。grep 无匹配、
// diff 有差异、test 条件为假、自定义脚本的业务退出码……各命令语义不同，且
// 无法在代码侧可靠枚举（白名单永远追不全，复合命令的退出码更无法归属）。
// 因此统一策略：任何非零退出一律正常返回，输出末尾附加 "Exit code: N" 标注，
// 判断权交给模型——stderr 与退出码注记双信号在场，模型漏判风险远低于把
// 正常结果误报成失败引发的重试与 || true 污染（agent-tool-issues 实测清单）。
//
// 仍按失败处理的例外（BashTool 分支，先于本判定）：
//   - 超时与用户取消；
//   - exitCode < 0：进程未启动、被信号终止，或错误混有输出读失败导致
//     输出完整性存疑、无可靠退出码，保留 error 路径。

// bashExitCode 从执行错误中取进程退出码；取不到（进程未启动、启动失败或
// 错误混有 stdout/stderr 读失败等成分导致输出完整性存疑）返回 -1，按失败处理。
func bashExitCode(runErr error) int {
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return -1
	}
	if joined, ok := runErr.(interface{ Unwrap() []error }); ok {
		if len(joined.Unwrap()) > 1 {
			return -1
		}
	}
	return exitErr.ExitCode()
}

// appendExitCodeNote 在输出末尾附加退出码标注：非零退出不打失败标签，
// 但模型仍需看到数值才能自行判断（如 grep exit 1 = 无匹配是查询结论）。
func appendExitCodeNote(output string, exitCode int) string {
	note := fmt.Sprintf("Exit code: %d", exitCode)
	trimmed := strings.TrimRight(output, "\n")
	if trimmed == "" {
		return note
	}
	return trimmed + "\n" + note
}

// bashChildEnv 构造 bash 子进程环境：继承宿主全部变量，并保证
// PYTHONIOENCODING=utf-8（setdefault 语义：环境里已有该变量时不覆盖用户
// 显式设置）。中文 Windows（ACP 936）下 python print 中文默认乱码；在环境
// 层兜底，让所有经 bash 执行的 python 脚本免掉每次的编码前处理。
func bashChildEnv(env []string) []string {
	for _, entry := range env {
		if strings.HasPrefix(entry, "PYTHONIOENCODING=") {
			return env
		}
	}
	return append(env, "PYTHONIOENCODING=utf-8")
}
