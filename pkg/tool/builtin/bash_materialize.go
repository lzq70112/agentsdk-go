package toolbuiltin

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// windowsNullRedirect 匹配 Windows 风格的空设备重定向（>nul / 2>nul / >>nul），
// 边界为空白、&、;、) 或行尾——不误伤 nul.txt 这类普通文件名。
var windowsNullRedirect = regexp.MustCompile(`(?i)(\d?>|>>)(\s*)nul(\s|&|;|\)|$)`)

// rewriteWindowsNullRedirect 把 payload 里的 Windows 空设备重定向改写为
// /dev/null，避免 bash 把 nul 当普通文件名创建。仅 Windows 生效（对齐
// Comate rewrite_windows_null_redirect，正则作用于全量多行 payload）。
func rewriteWindowsNullRedirect(payload string) string {
	if bashResolverGOOS() != "windows" {
		return payload
	}
	return windowsNullRedirect.ReplaceAllString(payload, "${1}${2}/dev/null${3}")
}

// normalizeBashNewlines 把 \r\n / \r 归一为 \n：从 Windows 复制的多行命令
// 也能正确执行，不让 \r 混进脚本字节。
func normalizeBashNewlines(command string) string {
	command = strings.ReplaceAll(command, "\r\n", "\n")
	return strings.ReplaceAll(command, "\r", "\n")
}

// bashMaterializeScript 把命令物化为临时脚本文件（UTF-8、LF、无 BOM、末尾
// 换行补齐），返回脚本绝对路径与幂等清理函数。多行命令因此绕开命令行传参
// 的转义与 Windows CreateProcess 32K 长度限制，脚本字节与模型输入逐字节
// 一致（仅 CRLF 归一化与 >nul 改写两处刻意变换）。执行后（含失败路径）
// 必须调用清理函数。
func bashMaterializeScript(command string) (string, func(), error) {
	payload := rewriteWindowsNullRedirect(normalizeBashNewlines(command))
	if !strings.HasSuffix(payload, "\n") {
		payload += "\n"
	}
	f, err := os.CreateTemp("", "agentsdk-bash-*.sh")
	if err != nil {
		return "", nil, fmt.Errorf("create bash script: %w", err)
	}
	path := f.Name()
	_, writeErr := f.WriteString(payload)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return "", nil, fmt.Errorf("write bash script %s: %w", path, errors.Join(writeErr, closeErr))
	}
	return path, func() { _ = os.Remove(path) }, nil
}
