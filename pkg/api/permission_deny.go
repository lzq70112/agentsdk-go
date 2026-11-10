package api

import (
	"regexp"
	"strings"
)

// denyKind 枚举 permissions.deny 规则的四种形态。
type denyKind int

const (
	denyBare     denyKind = iota // "Bash"：整个工具拒绝
	denyPrefix                   // "Bash(ssh:*)"：命令前缀匹配
	denyExact                    // "Bash(docker ps)"：整条命令精确匹配（空白归一化后）
	denyWildcard                 // "Bash(docker * ps)"：* 为通配段
)

// denyRule 是一条已编译的 deny 规则。selector 规则只对 bash 工具生效
// （匹配 params["command"]），其余工具仅支持裸工具名规则。
type denyRule struct {
	raw    string
	tool   string // canonical（小写）工具名
	kind   denyKind
	prefix string // denyPrefix 时的命令前缀（含 xargs 变体）
	exact  string // denyExact 时的归一化命令
	re     *regexp.Regexp
}

// compileDenyRules 把 deny 规则字符串编译为可匹配形式，返回无法执行的规则
// 清单（非 bash 工具的 selector 规则，以及语法畸形项）供调用方告警——静默
// 忽略会制造"已配置=已防护"的假象。
func compileDenyRules(rules []string) (compiled []denyRule, unenforceable []string) {
	for _, raw := range rules {
		rule := strings.TrimSpace(raw)
		if rule == "" {
			continue
		}
		open := strings.IndexRune(rule, '(')
		if open < 0 {
			compiled = append(compiled, denyRule{raw: rule, tool: canonicalToolName(rule), kind: denyBare})
			continue
		}
		if !strings.HasSuffix(rule, ")") || strings.Count(rule, "(") != 1 || strings.Count(rule, ")") != 1 {
			unenforceable = append(unenforceable, rule)
			continue
		}
		tool := canonicalToolName(rule[:open])
		target := strings.TrimSpace(rule[open+1 : len(rule)-1])
		if tool == "" || target == "" {
			unenforceable = append(unenforceable, rule)
			continue
		}
		if tool != "bash" {
			unenforceable = append(unenforceable, rule)
			continue
		}
		compiled = append(compiled, compileBashDenyRule(rule, target))
	}
	return compiled, unenforceable
}

func compileBashDenyRule(raw, target string) denyRule {
	switch {
	case strings.HasSuffix(target, ":*"):
		prefix := strings.TrimSpace(strings.TrimSuffix(target, ":*"))
		return denyRule{raw: raw, tool: "bash", kind: denyPrefix, prefix: prefix}
	case strings.ContainsRune(target, '*'):
		pattern := "^" + regexp.QuoteMeta(normalizeCommand(target)) + "$"
		pattern = strings.ReplaceAll(pattern, `\*`, ".*")
		re, err := regexp.Compile(pattern)
		if err != nil {
			return denyRule{raw: raw, tool: "bash", kind: denyWildcard}
		}
		return denyRule{raw: raw, tool: "bash", kind: denyWildcard, re: re}
	default:
		return denyRule{raw: raw, tool: "bash", kind: denyExact, exact: normalizeCommand(target)}
	}
}

// matchDeny 返回命中的规则原文。命中即拒：deny 是安全边界，宁可多拒。
func matchDeny(rules []denyRule, toolName string, params map[string]any) (string, bool) {
	if len(rules) == 0 {
		return "", false
	}
	canon := canonicalToolName(toolName)
	if canon == "" {
		return "", false
	}
	for _, rule := range rules {
		if rule.tool != canon {
			continue
		}
		if rule.kind == denyBare {
			return rule.raw, true
		}
		// selector 规则只对 bash 生效；命令取不到时按不匹配处理。
		if canon != "bash" {
			continue
		}
		cmd, _ := params["command"].(string)
		if cmd == "" {
			continue
		}
		if bashSegmentMatches(cmd, rule) {
			return rule.raw, true
		}
	}
	return "", false
}

// bashSegmentMatches 把命令按顶层分隔符切段后逐段匹配，任一段命中即真。
// 复合命令（echo a && ssh b）的 deny 语义是"任一段命中就整体拒绝"。
func bashSegmentMatches(cmd string, rule denyRule) bool {
	for _, seg := range splitShellSegments(cmd) {
		seg = normalizeCommand(stripEnvPrefixes(seg))
		if seg == "" {
			continue
		}
		switch rule.kind {
		case denyPrefix:
			if seg == rule.prefix || strings.HasPrefix(seg, rule.prefix+" ") {
				return true
			}
			// xargs 变体：`find . | xargs rm` 的实际命令是 rm。
			if seg == "xargs "+rule.prefix || strings.HasPrefix(seg, "xargs "+rule.prefix+" ") {
				return true
			}
		case denyExact:
			if seg == rule.exact {
				return true
			}
		case denyWildcard:
			if rule.re != nil && rule.re.MatchString(seg) {
				return true
			}
		}
	}
	return false
}

// splitShellSegments 按顶层（引号外）的 && || ; | & |& 切分命令。
// 引号内的分隔符不算（ssh host "cd /x && restart" 是一条 ssh 命令）。
func splitShellSegments(cmd string) []string {
	var (
		segs    []string
		cur     strings.Builder
		quote   byte
		escaped bool
	)
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			segs = append(segs, cur.String())
		}
		cur.Reset()
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case quote == '"' && c == '\\':
			cur.WriteByte(c)
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == '\\':
			cur.WriteByte(c)
			escaped = true
		case c == ';':
			flush()
		case c == '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				i++
			}
			flush()
		case c == '|':
			if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
				i++
			}
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

// envPrefixRe 匹配段首的环境变量赋值（VAR=value 形态，值支持裸 token 与引号形式）。
// 原生 ssh 调用常写成 `MSYS_NO_PATHCONV=1 ssh ...`，不剥前缀就拦不住。
var envPrefixRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|\S+)\s+`)

func stripEnvPrefixes(seg string) string {
	for {
		loc := envPrefixRe.FindStringIndex(seg)
		if loc == nil {
			return seg
		}
		seg = seg[loc[1]:]
	}
}

func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
