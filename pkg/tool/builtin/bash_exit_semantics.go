package toolbuiltin

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// Bash 非零退出码语义：判断一个非零退出码是否真的表示命令失败。
//
// POSIX 里 exit code != 0 表示「有话要说」，不表示「出错了」。grep 无匹配、
// diff 有差异、test 条件为假，都用 exit 1 表达正常结果。把这些一律判成错误，
// 模型会收到 "command failed: exit status 1" 的假告警，进而误以为环境或命令
// 本身有问题、盲目重试（agent-tool-issues 实测清单）。
//
// 设计取 Comate（comate_agent_sdk _bash_exit_semantics.py）的保守子集：
//   - 只有白名单命令按「失败阈值」判定（exit >= 阈值才算失败）；
//   - 未列出的命令沿用 POSIX 默认：非零即失败；
//   - 只有能可靠归属退出码的单条简单命令享受白名单；复合命令（&& || ; |）、
//     子 shell、命令替换、无法识别的形态一律保守按失败处理，避免把真实失败
//     改写成成功结果。
var bashErrorExitThresholds = map[string]int{
	"grep": 2, // 1 = 无匹配
	"rg":   2, // ripgrep 同 grep
	"diff": 2, // 1 = 文件有差异
	"test": 2, // 1 = 条件为假
	"[":    2, // test 的别名
}

// envAssignmentRe 匹配 env 的 VAR=value 参数。
var envAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=.*$`)

// isBashExitError 判定该退出码是否应作为工具失败返回（err != nil）。
// exitCode < 0 是内部哨兵（进程未启动或取不到退出码），一律按失败处理。
func isBashExitError(command string, exitCode int) bool {
	if exitCode == 0 {
		return false
	}
	if exitCode < 0 {
		return true
	}
	base := bashBaseCommand(command)
	if base == "" {
		return true
	}
	threshold, listed := bashErrorExitThresholds[base]
	if !listed {
		return true
	}
	return exitCode >= threshold
}

// bashBaseCommand 取真正决定退出码的简单命令名；无法可靠判定时返回空串。
func bashBaseCommand(command string) string {
	words := bashSimpleCommandWords(command)
	if len(words) == 0 {
		return ""
	}
	// 透明包装器剥离：`command` 与 `env` 只在位于最外层时生效。
	switch words[0] {
	case "command":
		words = unwrapBashCommand(words)
	case "env":
		words = unwrapBashEnv(words)
	}
	if len(words) == 0 {
		return ""
	}
	return normalizeBashCommandName(words[0])
}

// bashSimpleCommandWords 把命令拆成引号感知的词序列；只接受不含任何 shell
// 控制操作符的单条简单命令（重定向除外——它不改变退出码归属），其余返回 nil。
func bashSimpleCommandWords(command string) []string {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return nil
	}

	var words []string
	var current strings.Builder
	var wordStarted bool
	var inSingle, inDouble bool
	flush := func() {
		if wordStarted {
			words = append(words, current.String())
			current.Reset()
			wordStarted = false
		}
	}

	for _, r := range trimmed {
		switch {
		case r == '\\' && !inSingle:
			// 转义字符：原样保留给分词结果，转义符不属于控制操作符。
			current.WriteRune(r)
			wordStarted = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
			wordStarted = true
		case r == '"' && !inSingle:
			inDouble = !inDouble
			wordStarted = true
		case (inSingle || inDouble):
			current.WriteRune(r)
			wordStarted = true
		case strings.ContainsRune("&|;`{(", r):
			// 未引用的控制操作符 / 子 shell / 命令替换：退出码无法归属，保守放弃。
			return nil
		case r == '$':
			// 命令替换 $( 与变量展开都可能改变命令本体；首个词形如 $CMD 时
			// 无法静态归属。出现在词中间的变量（如 grep $PAT f）不影响首个
			// 词的归属，放行。
			if !wordStarted {
				return nil
			}
			current.WriteRune(r)
			wordStarted = true
		case r == '>' || r == '<':
			// 重定向不改变退出码归属：跳过，直到下一个空白（引号已闭合）。
			// 简化处理：把重定向操作符当分隔符，其目标文件路径会成为独立词，
			// 由后续「首个词即命令名」的语义天然排除。
			flush()
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			current.WriteRune(r)
			wordStarted = true
		}
	}
	if inSingle || inDouble {
		return nil
	}
	flush()
	if len(words) == 0 {
		return nil
	}
	return words
}

// unwrapBashCommand 剥离 `command` 的选项（仅 -p）；遇到 `--` 后的词或无法
// 识别的选项返回 nil（保守放弃白名单资格）。
func unwrapBashCommand(words []string) []string {
	index := 1
	for index < len(words) {
		word := words[index]
		if word == "--" {
			index++
			break
		}
		if word == "-p" {
			index++
			continue
		}
		if strings.HasPrefix(word, "-") {
			return nil
		}
		break
	}
	if index >= len(words) {
		return nil
	}
	return words[index:]
}

// unwrapBashEnv 剥离 `env` 的选项（-i / -u NAME）与 VAR=value 赋值；
// 选项顺序敏感形态（如合并短选项）无法可靠解析时返回 nil。
func unwrapBashEnv(words []string) []string {
	index := 1
	for index < len(words) {
		word := words[index]
		if word == "--" {
			index++
			break
		}
		if word == "-i" {
			index++
			continue
		}
		if word == "-u" {
			if index+1 >= len(words) {
				return nil
			}
			index += 2
			continue
		}
		if strings.HasPrefix(word, "-") {
			return nil
		}
		if envAssignmentRe.MatchString(word) {
			index++
			continue
		}
		break
	}
	if index >= len(words) {
		return nil
	}
	return words[index:]
}

// normalizeBashCommandName 把可执行 token 规范化为白名单使用的命令名：
// 去路径、去 .exe 后缀、Windows 下小写化。
func normalizeBashCommandName(token string) string {
	name := token
	if idx := strings.LastIndexAny(name, `/\`); idx >= 0 {
		name = name[idx+1:]
	}
	if lower := strings.ToLower(name); strings.HasSuffix(lower, ".exe") {
		name = name[:len(name)-len(".exe")]
	}
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
	}
	return name
}

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

// appendExitCodeNote 在输出末尾附加退出码标注：benign 退出不打失败标签，
// 但模型仍需看到数值才能自行判断（如 grep exit 1 = 无匹配是查询结论）。
func appendExitCodeNote(output string, exitCode int) string {
	note := fmt.Sprintf("Exit code: %d", exitCode)
	trimmed := strings.TrimRight(output, "\n")
	if trimmed == "" {
		return note
	}
	return trimmed + "\n" + note
}
