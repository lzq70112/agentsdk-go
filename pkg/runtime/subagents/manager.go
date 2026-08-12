package subagents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
)

const (
	TypeGeneralPurpose = "general-purpose"
	TypeExplore        = "explore"
	TypePlan           = "plan"

	ModelSonnet                    = "sonnet"
	ModelHaiku                     = "haiku"
	defaultMaxConcurrentBackground = 3
)

var (
	ErrDuplicateSubagent  = errors.New("subagents: duplicate registration")
	ErrUnknownSubagent    = errors.New("subagents: unknown target")
	ErrNoMatchingSubagent = errors.New("subagents: no matching subagent")
	ErrEmptyInstruction   = errors.New("subagents: instruction is empty")
	ErrUnknownTask        = errors.New("subagents: unknown task")
)

type StatusState string

const (
	StatusQueued  StatusState = "queued"
	StatusRunning StatusState = "running"
	StatusSuccess StatusState = "success"
	StatusError   StatusState = "error"
)

var builtinSubagentTypes = map[string]Definition{
	TypeGeneralPurpose: {
		Name:         TypeGeneralPurpose,
		Description:  "General-purpose agent for complex reasoning, research, and coding tasks.",
		DefaultModel: ModelSonnet,
		BaseContext: Context{
			Model: ModelSonnet,
		},
	},
	TypeExplore: {
		Name:         TypeExplore,
		Description:  "Fast explorer limited to Glob/Grep/Read for targeted code navigation and Q&A.",
		DefaultModel: ModelHaiku,
		BaseContext: Context{
			ToolWhitelist: []string{"glob", "grep", "read"},
			Model:         ModelHaiku,
		},
	},
	TypePlan: {
		Name:         TypePlan,
		Description:  "Planning agent focused on outlining multi-step strategies with full tool access.",
		DefaultModel: ModelSonnet,
		BaseContext: Context{
			Model: ModelSonnet,
		},
	},
}

// BuiltinDefinitions returns the predefined metadata for core subagent types.
func BuiltinDefinitions() []Definition {
	keys := make([]string, 0, len(builtinSubagentTypes))
	for name := range builtinSubagentTypes {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	defs := make([]Definition, 0, len(keys))
	for _, name := range keys {
		defs = append(defs, cloneDefinition(builtinSubagentTypes[name]))
	}
	return defs
}

// BuiltinDefinition looks up a predefined subagent type by name.
func BuiltinDefinition(name string) (Definition, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	def, ok := builtinSubagentTypes[key]
	if !ok {
		return Definition{}, false
	}
	return cloneDefinition(def), true
}

// Definition describes a single subagent.
type Definition struct {
	Name         string
	Description  string
	Priority     int
	MutexKey     string
	BaseContext  Context
	Matchers     []skills.Matcher
	DefaultModel string
}

// Validate ensures the definition is safe to register.
func (d Definition) Validate() error {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		return errors.New("subagents: name is required")
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return fmt.Errorf("subagents: invalid name %q", d.Name)
		}
	}
	return nil
}

// Handler executes a subagent request.
type Handler interface {
	Handle(context.Context, Context, Request) (Result, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Context, Request) (Result, error)

// Handle implements Handler.
func (fn HandlerFunc) Handle(ctx context.Context, subCtx Context, req Request) (Result, error) {
	if fn == nil {
		return Result{}, errors.New("subagents: handler func is nil")
	}
	return fn(ctx, subCtx, req)
}

// Request carries execution parameters for a subagent run.
type Request struct {
	Target        string
	Instruction   string
	Activation    skills.ActivationContext
	ToolWhitelist []string
	Metadata      map[string]any
}

// Result captures handler output.
type Result struct {
	Subagent string
	Output   any
	Metadata map[string]any
	Error    string
}

func (r Result) clone() Result {
	if len(r.Metadata) > 0 {
		r.Metadata = maps.Clone(r.Metadata)
	}
	return r
}

type Status struct {
	TaskID      string
	Name        string
	Instruction string
	SessionID   string
	State       StatusState
	Result      Result
	Output      string
	Error       string
}

func (s Status) clone() Status {
	s.Result = s.Result.clone()
	return s
}

type Manager struct {
	mu                      sync.RWMutex
	subagents               map[string]*registeredSubagent
	tasks                   map[string]Status
	backgroundSlots         chan struct{}
	maxConcurrentBackground int
	onComplete              func(Status)
	nextTaskID              uint64
}

// NewManager builds a new manager.
func NewManager() *Manager {
	return &Manager{
		subagents:               map[string]*registeredSubagent{},
		tasks:                   map[string]Status{},
		backgroundSlots:         make(chan struct{}, defaultMaxConcurrentBackground),
		maxConcurrentBackground: defaultMaxConcurrentBackground,
	}
}

// Register installs a subagent definition + handler.
func (m *Manager) Register(def Definition, handler Handler) error {
	if err := def.Validate(); err != nil {
		return err
	}
	if handler == nil {
		return errors.New("subagents: handler is nil")
	}
	baseCtx := def.BaseContext.Clone()
	baseCtx.Model = strings.TrimSpace(baseCtx.Model)
	if baseCtx.Model == "" {
		baseCtx.Model = strings.TrimSpace(def.DefaultModel)
	}
	normalized := registeredSubagent{
		definition: Definition{
			Name:         strings.ToLower(strings.TrimSpace(def.Name)),
			Description:  strings.TrimSpace(def.Description),
			Priority:     max(def.Priority, 0),
			MutexKey:     strings.ToLower(strings.TrimSpace(def.MutexKey)),
			BaseContext:  baseCtx,
			Matchers:     append([]skills.Matcher(nil), def.Matchers...),
			DefaultModel: strings.TrimSpace(def.DefaultModel),
		},
		handler: handler,
	}
	key := normalized.definition.Name

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.subagents[key]; exists {
		return ErrDuplicateSubagent
	}
	m.subagents[key] = &normalized
	return nil
}

// List returns registered subagent definitions sorted by priority + name.
func (m *Manager) List() []Definition {
	m.mu.RLock()
	defs := make([]Definition, 0, len(m.subagents))
	for _, sub := range m.subagents {
		defs = append(defs, cloneDefinition(sub.definition))
	}
	m.mu.RUnlock()
	sort.Slice(defs, func(i, j int) bool {
		if defs[i].Priority != defs[j].Priority {
			return defs[i].Priority > defs[j].Priority
		}
		return defs[i].Name < defs[j].Name
	})
	return defs
}

// Dispatch selects and executes a subagent. When Target is empty, automatic
// matchers choose the best candidate subject to priority/mutex ordering.
func (m *Manager) Dispatch(ctx context.Context, req Request) (Result, error) {
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		return Result{}, ErrEmptyInstruction
	}
	target, err := m.selectTarget(req)
	if err != nil {
		return Result{}, err
	}
	runCtx := target.definition.BaseContext.Clone()
	if inherited, ok := FromContext(ctx); ok {
		runCtx = mergeContext(runCtx, inherited)
	}
	if len(req.Metadata) > 0 {
		runCtx = runCtx.WithMetadata(req.Metadata)
	}
	if sessionID, ok := req.Metadata["session_id"].(string); ok {
		runCtx = runCtx.WithSession(sessionID)
	}
	if len(req.ToolWhitelist) > 0 {
		runCtx = runCtx.RestrictTools(req.ToolWhitelist...)
	}

	result, execErr := target.handler.Handle(ctx, runCtx, req)
	result.Subagent = target.definition.Name
	result = result.clone()
	if execErr != nil {
		result.Error = execErr.Error()
		return result, execErr
	}
	return result, nil
}

func (m *Manager) SetCompletionHandler(fn func(Status)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onComplete = fn
}

func (m *Manager) SetMaxConcurrentBackground(n int) {
	if m == nil {
		return
	}
	if n <= 0 {
		n = defaultMaxConcurrentBackground
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxConcurrentBackground = n
	m.backgroundSlots = make(chan struct{}, n)
}

func (m *Manager) DispatchAsync(ctx context.Context, name, instruction string) (string, error) {
	return m.dispatchAsync(ctx, Request{Target: name, Instruction: instruction})
}

func (m *Manager) TaskStatus(taskID string) (Status, error) {
	if m == nil {
		return Status{}, ErrUnknownTask
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return Status{}, ErrUnknownTask
	}
	m.mu.RLock()
	status, ok := m.tasks[taskID]
	m.mu.RUnlock()
	if !ok {
		return Status{}, ErrUnknownTask
	}
	return status.clone(), nil
}

func (m *Manager) dispatchAsync(ctx context.Context, req Request) (string, error) {
	if strings.TrimSpace(req.Instruction) == "" {
		return "", ErrEmptyInstruction
	}
	taskID := fmt.Sprintf("task-%d", atomic.AddUint64(&m.nextTaskID, 1))
	status := Status{
		TaskID:      taskID,
		Name:        strings.ToLower(strings.TrimSpace(req.Target)),
		Instruction: strings.TrimSpace(req.Instruction),
		SessionID:   sessionID(ctx, req.Metadata),
		State:       StatusQueued,
	}

	m.mu.Lock()
	m.tasks[taskID] = status
	sem := m.backgroundSlots
	onComplete := m.onComplete
	m.mu.Unlock()

	go func() {
		dispatchCtx := ctx
		if dispatchCtx == nil {
			dispatchCtx = context.Background()
		}

		select {
		case sem <- struct{}{}:
		case <-dispatchCtx.Done():
			m.completeTask(Status{
				TaskID:      taskID,
				Name:        status.Name,
				Instruction: status.Instruction,
				SessionID:   status.SessionID,
				State:       StatusError,
				Error:       dispatchCtx.Err().Error(),
			}, onComplete)
			return
		}
		defer func() { <-sem }()

		m.updateTask(taskID, func(current *Status) {
			current.State = StatusRunning
		})

		result, err := m.Dispatch(dispatchCtx, req)
		output := ""
		if result.Output != nil {
			output = strings.TrimSpace(fmt.Sprint(result.Output))
		}
		final := Status{
			TaskID:      taskID,
			Name:        result.Subagent,
			Instruction: status.Instruction,
			SessionID:   status.SessionID,
			Result:      result.clone(),
			Output:      output,
			Error:       strings.TrimSpace(result.Error),
			State:       StatusSuccess,
		}
		if final.Name == "" {
			final.Name = status.Name
		}
		if err != nil {
			final.State = StatusError
			if final.Error == "" {
				final.Error = err.Error()
			}
		}
		m.completeTask(final, onComplete)
	}()

	return taskID, nil
}

func (m *Manager) selectTarget(req Request) (*registeredSubagent, error) {
	if target := strings.TrimSpace(req.Target); target != "" {
		m.mu.RLock()
		sub, ok := m.subagents[strings.ToLower(target)]
		m.mu.RUnlock()
		if !ok {
			return nil, ErrUnknownSubagent
		}
		return sub, nil
	}
	matches := m.matching(req.Activation)
	if len(matches) == 0 {
		return nil, ErrNoMatchingSubagent
	}
	return matches[0], nil
}

func (m *Manager) matching(ctx skills.ActivationContext) []*registeredSubagent {
	m.mu.RLock()
	snapshot := make([]*registeredSubagent, 0, len(m.subagents))
	for _, sub := range m.subagents {
		snapshot = append(snapshot, sub)
	}
	m.mu.RUnlock()

	type candidate struct {
		sub   *registeredSubagent
		score float64
	}
	var candidates []candidate
	for _, sub := range snapshot {
		if len(sub.definition.Matchers) == 0 {
			candidates = append(candidates, candidate{sub, 0.5})
			continue
		}
		var best skills.MatchResult
		matched := false
		for _, matcher := range sub.definition.Matchers {
			if matcher == nil {
				continue
			}
			result := matcher.Match(ctx)
			if !result.Matched {
				continue
			}
			if !matched || result.BetterThan(best) {
				best = result
				matched = true
			}
		}
		if !matched {
			continue
		}
		candidates = append(candidates, candidate{sub: sub, score: best.Score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		di := candidates[i].sub.definition
		dj := candidates[j].sub.definition
		if di.Priority != dj.Priority {
			return di.Priority > dj.Priority
		}
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		return di.Name < dj.Name
	})

	seen := map[string]struct{}{}
	filtered := make([]*registeredSubagent, 0, len(candidates))
	for _, cand := range candidates {
		key := cand.sub.definition.MutexKey
		if key == "" {
			filtered = append(filtered, cand.sub)
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, cand.sub)
	}
	return filtered
}

func (m *Manager) updateTask(taskID string, update func(*Status)) {
	if update == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.tasks[taskID]
	if !ok {
		return
	}
	update(&current)
	m.tasks[taskID] = current
}

func (m *Manager) completeTask(status Status, onComplete func(Status)) {
	status = status.clone()
	m.mu.Lock()
	m.tasks[status.TaskID] = status
	m.mu.Unlock()
	if onComplete != nil {
		onComplete(status.clone())
	}
}

func mergeContext(base Context, inherited Context) Context {
	if session := strings.TrimSpace(inherited.SessionID); session != "" {
		base.SessionID = session
	}
	base = base.WithMetadata(inherited.Metadata)
	if len(inherited.ToolWhitelist) > 0 {
		base = base.RestrictTools(inherited.ToolWhitelist...)
	}
	if model := strings.TrimSpace(inherited.Model); model != "" {
		base.Model = model
	}
	return base
}

func sessionID(ctx context.Context, meta map[string]any) string {
	if inherited, ok := FromContext(ctx); ok {
		if session := strings.TrimSpace(inherited.SessionID); session != "" {
			return session
		}
	}
	if raw, ok := meta["session_id"].(string); ok {
		return strings.TrimSpace(raw)
	}
	return ""
}

func cloneDefinition(def Definition) Definition {
	cloned := Definition{
		Name:         def.Name,
		Description:  def.Description,
		Priority:     def.Priority,
		MutexKey:     def.MutexKey,
		BaseContext:  def.BaseContext.Clone(),
		Matchers:     append([]skills.Matcher(nil), def.Matchers...),
		DefaultModel: def.DefaultModel,
	}
	return cloned
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type registeredSubagent struct {
	definition Definition
	handler    Handler
}
