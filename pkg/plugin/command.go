package plugin

import (
	"fmt"
	"strings"
	"unicode"
)

// Command is one commands/<name>.md definition: YAML frontmatter metadata plus
// a prompt template body. The SDK only stores and renders commands; deciding
// when to turn one into a prompt is up to the host (e.g. IM slash messages).
type Command struct {
	Name         string // file name without extension, lowercased
	Description  string
	ArgumentHint string
	Template     string
}

// Render substitutes $ARGUMENTS (whole argument string) and $1..$9 (positional
// arguments split on whitespace; out-of-range becomes empty) in the template.
func (c Command) Render(args string) string {
	out := strings.ReplaceAll(c.Template, "$ARGUMENTS", args)
	fields := strings.Fields(args)
	for i := 0; i < 9; i++ {
		token := "$" + fmt.Sprint(i+1)
		val := ""
		if i < len(fields) {
			val = fields[i]
		}
		out = strings.ReplaceAll(out, token, val)
	}
	return out
}

// parseCommandFile splits a commands/*.md file into frontmatter metadata and
// template body. Frontmatter is optional for commands; only the description
// and argument-hint keys are consumed.
func parseCommandFile(content string) (Command, error) {
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	if !strings.HasPrefix(strings.TrimSpace(trimmed), "---") {
		return Command{Template: strings.TrimSpace(trimmed)}, nil
	}
	lines := strings.Split(trimmed, "\n")
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return Command{}, fmt.Errorf("plugin: command frontmatter not closed")
	}
	meta := map[string]string{}
	for _, line := range lines[1:end] {
		key, val, ok := splitYAMLSimple(line)
		if !ok {
			continue
		}
		meta[key] = val
	}
	body := strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	return Command{
		Description:  meta["description"],
		ArgumentHint: meta["argument-hint"],
		Template:     body,
	}, nil
}

// splitYAMLSimple parses "key: value" / "key:" lines without pulling in a YAML
// dependency for the two string keys commands use.
func splitYAMLSimple(line string) (string, string, bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key := strings.ToLower(strings.TrimSpace(line[:idx]))
	val := strings.TrimSpace(line[idx+1:])
	val = strings.Trim(val, `"'`)
	if !isSimpleKey(key) {
		return "", "", false
	}
	return key, val, true
}

func isSimpleKey(key string) bool {
	for _, r := range key {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' {
			return false
		}
	}
	return key != ""
}
