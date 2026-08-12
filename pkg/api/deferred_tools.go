package api

import (
	"sort"
	"strings"
	"sync"

	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

type deferredToolState struct {
	mu       sync.RWMutex
	tools    map[string]tool.Tool
	sessions map[string]map[string]struct{}
}

func newDeferredToolState(registry *tool.Registry) *deferredToolState {
	if registry == nil {
		return nil
	}
	tools := map[string]tool.Tool{}
	for _, impl := range registry.List() {
		if impl == nil || !tool.ShouldDefer(impl) {
			continue
		}
		key := canonicalToolName(impl.Name())
		if key == "" {
			continue
		}
		tools[key] = impl
	}
	if len(tools) == 0 {
		return nil
	}
	return &deferredToolState{
		tools:    tools,
		sessions: map[string]map[string]struct{}{},
	}
}

func (s *deferredToolState) shouldExpose(name, sessionID string) bool {
	if s == nil {
		return true
	}
	key := canonicalToolName(name)
	if _, ok := s.tools[key]; !ok {
		return true
	}
	if sessionID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if active := s.sessions[sessionID]; active != nil {
		_, ok := active[key]
		return ok
	}
	return false
}

func (s *deferredToolState) activate(sessionID string, names []string) {
	if s == nil || sessionID == "" || len(names) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.sessions[sessionID]
	if active == nil {
		active = map[string]struct{}{}
		s.sessions[sessionID] = active
	}
	for _, name := range names {
		key := canonicalToolName(name)
		if key == "" {
			continue
		}
		if _, ok := s.tools[key]; ok {
			active[key] = struct{}{}
		}
	}
}

func (s *deferredToolState) inactiveNames(sessionID string, whitelist map[string]struct{}) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	active := s.sessions[sessionID]
	names := make([]string, 0, len(s.tools))
	for key, impl := range s.tools {
		if len(whitelist) > 0 {
			if _, ok := whitelist[key]; !ok {
				continue
			}
		}
		if active != nil {
			if _, ok := active[key]; ok {
				continue
			}
		}
		names = append(names, strings.TrimSpace(impl.Name()))
	}
	sort.Strings(names)
	return names
}

func deferredToolSection(names []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<available-deferred-tools>\n")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	b.WriteString("</available-deferred-tools>")
	return b.String()
}
