package api

import (
	"sort"
	"strings"
)

const (
	SystemPromptSectionIdentity        = "identity"
	SystemPromptSectionRules           = "rules"
	SystemPromptSectionSkills          = "skills"
	SystemPromptSectionTools           = "tools"
	SystemPromptSectionMemory          = "memory"
	SystemPromptSectionMCPInstructions = "mcp-instructions"

	SystemPromptPriorityIdentity        = 0
	SystemPromptPriorityRules           = 10
	SystemPromptPrioritySkills          = 20
	SystemPromptPriorityTools           = 30
	SystemPromptPriorityMemory          = 40
	SystemPromptPriorityMCPInstructions = 50
)

type promptSection struct {
	Name     string
	Content  string
	Priority int
	Order    int
}

type SystemPromptBuilder struct {
	sections  map[string]promptSection
	nextOrder int
}

func NewSystemPromptBuilder() *SystemPromptBuilder {
	return &SystemPromptBuilder{sections: map[string]promptSection{}}
}

func (b *SystemPromptBuilder) Clone() *SystemPromptBuilder {
	if b == nil {
		return nil
	}
	clone := &SystemPromptBuilder{
		sections:  make(map[string]promptSection, len(b.sections)),
		nextOrder: b.nextOrder,
	}
	for k, v := range b.sections {
		clone.sections[k] = v
	}
	return clone
}

func (b *SystemPromptBuilder) AddSection(name string, content string, priority int) {
	if b == nil {
		return
	}
	name = strings.TrimSpace(name)
	content = strings.TrimSpace(content)
	if name == "" {
		return
	}
	if b.sections == nil {
		b.sections = map[string]promptSection{}
	}
	section, ok := b.sections[name]
	if !ok {
		section.Order = b.nextOrder
		b.nextOrder++
	}
	section.Name = name
	section.Content = content
	section.Priority = priority
	b.sections[name] = section
}

func (b *SystemPromptBuilder) RemoveSection(name string) {
	if b == nil || len(b.sections) == 0 {
		return
	}
	delete(b.sections, strings.TrimSpace(name))
}

func (b *SystemPromptBuilder) Build() string {
	if b == nil || len(b.sections) == 0 {
		return ""
	}
	sections := make([]promptSection, 0, len(b.sections))
	for _, section := range b.sections {
		if strings.TrimSpace(section.Content) == "" {
			continue
		}
		sections = append(sections, section)
	}
	sort.Slice(sections, func(i, j int) bool {
		if sections[i].Priority != sections[j].Priority {
			return sections[i].Priority < sections[j].Priority
		}
		if sections[i].Order != sections[j].Order {
			return sections[i].Order < sections[j].Order
		}
		return sections[i].Name < sections[j].Name
	})
	parts := make([]string, 0, len(sections))
	for _, section := range sections {
		parts = append(parts, strings.TrimSpace(section.Content))
	}
	return strings.Join(parts, "\n\n")
}
