package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stellarlinkco/agentsdk-go/pkg/config"
	hooks "github.com/stellarlinkco/agentsdk-go/pkg/hooks"
	"github.com/stellarlinkco/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/agentsdk-go/pkg/runtime/skills"
	"github.com/stellarlinkco/agentsdk-go/pkg/sandbox"
	"github.com/stellarlinkco/agentsdk-go/pkg/tool"
)

var newTracer = NewTracer

type streamContextKey string

const streamEmitCtxKey streamContextKey = "agentsdk.stream.emit"

// streamForwardState 跟踪一次 message 生命周期内的真 delta 透传状态。
// 生命周期单位是 message（BeforeAgent → 0 或多次 completeOnce（含升级/压缩）→ AfterAgent），
// 故状态在 BeforeAgent 重置、在 AfterAgent 收尾。
//
// 字段：
//   - forwarded: 底层 CompleteStream 是否已透传真 delta。AfterAgent 据此跳过 textBlock 假重放。
//   - started:   是否已为本次 message 发出 content_block_start（idx=0）。
//     升级场景下 completeOnce 会多次调用 CompleteStream，但一个 text 块只 start 一次。
type streamForwardState struct {
	forwarded atomic.Bool
	started   atomic.Bool
}

const streamForwardStateCtxKey streamContextKey = "agentsdk.delta.forward.state"

func withStreamEmit(ctx context.Context, emit streamEmitFunc) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, streamEmitCtxKey, emit)
}

func streamEmitFromContext(ctx context.Context) streamEmitFunc {
	if ctx == nil {
		return nil
	}
	if emit, ok := ctx.Value(streamEmitCtxKey).(streamEmitFunc); ok {
		return emit
	}
	return nil
}

// Runtime exposes the unified SDK surface that powers CLI/CI/enterprise entrypoints.
type Runtime struct {
	opts      Options
	sbRoot    string
	registry  *tool.Registry
	executor  *tool.Executor
	hooks     *hooks.Executor
	histories *historyStore
	compactor *compactor
	deferred  *deferredToolState

	mu sync.RWMutex

	runMu     sync.Mutex
	runWG     sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	closed    bool
}

// New instantiates a unified runtime bound to the provided options.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	opts = opts.withDefaults()
	opts = opts.frozen()

	// 初始化文件系统抽象层
	fsLayer := config.NewFS(opts.ProjectRoot, opts.EmbedFS)
	opts.fsLayer = fsLayer

	if err := materializeEmbeddedClaudeHooks(opts.ProjectRoot, opts.EmbedFS); err != nil {
		log.Printf("claude hooks materializer warning: %v", err)
	}

	builder := opts.SystemPromptBuilder
	if builder == nil {
		builder = NewSystemPromptBuilder()
	} else {
		builder = builder.Clone()
	}
	if text := strings.TrimSpace(opts.SystemPrompt); text != "" {
		builder.AddSection(SystemPromptSectionIdentity, text, SystemPromptPriorityIdentity)
	}

	if memory, err := config.LoadAgentsMD(opts.ProjectRoot, fsLayer); err != nil {
		log.Printf("agents.md loader warning: %v", err)
	} else if strings.TrimSpace(memory) != "" {
		builder.AddSection(SystemPromptSectionMemory, fmt.Sprintf("## Memory\n\n%s", strings.TrimSpace(memory)), SystemPromptPriorityMemory)
	}

	settings, err := loadSettings(opts)
	if err != nil {
		return nil, err
	}
	opts.settingsSnapshot = settings

	mdl, err := resolveModel(ctx, opts)
	if err != nil {
		return nil, err
	}
	opts.Model = mdl

	sbox, sbRoot := buildSandboxManager(opts, settings)

	skReg, skErrs := buildSkillsRegistry(opts)
	for _, err := range skErrs {
		log.Printf("skill loader warning: %v", err)
	}
	opts.skReg = skReg

	subMgr, subErrs := buildSubagentsManager(opts)
	for _, err := range subErrs {
		log.Printf("subagent loader warning: %v", err)
	}
	opts.subMgr = subMgr

	registry := tool.NewRegistry()
	if err := registerTools(registry, opts, settings, opts.skReg); err != nil {
		return nil, err
	}
	mcpServers := collectMCPServers(settings, opts.MCPServers)
	if err := registerMCPServers(ctx, registry, sbox, mcpServers); err != nil {
		return nil, err
	}
	executor := tool.NewExecutor(registry, sbox).
		WithOutputPersister(tool.NewOutputPersister()).
		WithMaxOutputSize(opts.MaxToolOutputSize)

	hooks := newHookExecutor(opts, settings)
	compactor := newCompactor(opts.AutoCompact, opts.TokenLimit)

	tracer, err := newTracer(opts.OTEL)
	if err != nil {
		return nil, fmt.Errorf("otel tracer init: %w", err)
	}
	opts.tracer = tracer

	if opts.RulesEnabled == nil || (opts.RulesEnabled != nil && *opts.RulesEnabled) {
		loader := config.NewRulesLoader(opts.ProjectRoot)
		if _, err := loader.LoadRules(); err != nil {
			log.Printf("rules loader warning: %v", err)
		} else if rules := strings.TrimSpace(loader.GetContent()); rules != "" {
			builder.AddSection(SystemPromptSectionRules, fmt.Sprintf("## Project Rules\n\n%s", rules), SystemPromptPriorityRules)
		}
		if err := loader.Close(); err != nil {
			log.Printf("rules loader close warning: %v", err)
		}
	}
	opts.SystemPromptBuilder = builder
	opts.SystemPrompt = builder.Build()

	histories := newHistoryStore(opts.MaxSessions, opts.HistoryLoader)

	rt := &Runtime{
		opts:      opts,
		sbRoot:    sbRoot,
		registry:  registry,
		executor:  executor,
		hooks:     hooks,
		histories: histories,
		compactor: compactor,
		deferred:  newDeferredToolState(registry),
	}
	rt.bindSubagentCallbacks()
	return rt, nil
}

func (rt *Runtime) beginRun() error {
	rt.runMu.Lock()
	defer rt.runMu.Unlock()
	if rt.closed {
		return ErrRuntimeClosed
	}
	rt.runWG.Add(1)
	return nil
}

func (rt *Runtime) endRun() {
	rt.runWG.Done()
}

// Run executes the unified pipeline synchronously.
func (rt *Runtime) Run(ctx context.Context, req Request) (*Response, error) {
	if rt == nil {
		return nil, ErrRuntimeClosed
	}
	if err := rt.beginRun(); err != nil {
		return nil, err
	}
	defer rt.endRun()

	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		mode := rt.opts.modeContext()
		sessionID = defaultSessionID(mode.EntryPoint)
	}
	req.SessionID = sessionID

	prep, err := rt.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	result, err := rt.runAgent(prep)
	if err != nil {
		return nil, err
	}
	return rt.buildResponse(prep, result), nil
}

// RunStream executes the pipeline asynchronously and returns events over a channel.
func (rt *Runtime) RunStream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	if rt == nil {
		return nil, ErrRuntimeClosed
	}
	if strings.TrimSpace(req.Prompt) == "" && len(req.ContentBlocks) == 0 {
		return nil, errors.New("api: prompt is empty")
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		mode := rt.opts.modeContext()
		sessionID = defaultSessionID(mode.EntryPoint)
	}
	req.SessionID = sessionID

	if err := rt.beginRun(); err != nil {
		return nil, err
	}

	// 缓冲区增大以吸收前端延迟（逐字符渲染等）导致的背压，避免 progress emit 阻塞工具执行
	out := make(chan StreamEvent, 512)
	progressChan := make(chan StreamEvent, 256)
	baseCtx := ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	progressMW := newProgressMiddleware(progressChan)
	ctxWithEmit := withStreamEmit(baseCtx, progressMW.streamEmit())
	// 注入真 delta 透传状态：BeforeAgent 重置、回调透传 delta 时置位、AfterAgent 据此跳过假重放。
	ctxWithEmit = context.WithValue(ctxWithEmit, streamForwardStateCtxKey, &streamForwardState{})
	go func() {
		defer rt.endRun()
		defer close(out)

		prep, err := rt.prepare(ctxWithEmit, req)
		if err != nil {
			isErr := true
			out <- StreamEvent{Type: EventError, Output: err.Error(), IsError: &isErr}
			return
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			dropping := false
			for event := range progressChan {
				if dropping {
					continue
				}
				select {
				case out <- event:
				case <-ctxWithEmit.Done():
					dropping = true
				}
			}
		}()

		var runErr error
		var result runResult
		defer func() {
			if rt.hooks != nil {
				reason := "completed"
				if runErr != nil {
					reason = "error"
				}
				//nolint:errcheck // session end events are non-critical notifications
				rt.hooks.Publish(hooks.Event{
					Type:      hooks.SessionEnd,
					SessionID: req.SessionID,
					Payload:   hooks.SessionEndPayload{SessionID: req.SessionID, Reason: reason},
				})
			}
		}()

		result, runErr = rt.runAgentWithMiddleware(prep, progressMW)
		close(progressChan)
		<-done

		if runErr != nil {
			isErr := true
			out <- StreamEvent{Type: EventError, Output: runErr.Error(), IsError: &isErr}
			return
		}
		rt.buildResponse(prep, result)
	}()
	return out, nil
}

// Close releases held resources.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	rt.closeOnce.Do(func() {
		rt.runMu.Lock()
		rt.closed = true
		rt.runMu.Unlock()

		rt.runWG.Wait()

		var err error
		if rt.histories != nil {
			for _, sessionID := range rt.histories.SessionIDs() {
				if cleanupErr := cleanupBashOutputSessionDir(sessionID); cleanupErr != nil {
					log.Printf("api: session %q temp cleanup failed: %v", sessionID, cleanupErr)
				}
				if cleanupErr := cleanupToolOutputSessionDir(sessionID); cleanupErr != nil {
					log.Printf("api: session %q tool output cleanup failed: %v", sessionID, cleanupErr)
				}
			}
		}
		if rt.registry != nil {
			rt.registry.Close()
		}
		if rt.opts.tracer != nil {
			if e := rt.opts.tracer.Shutdown(); e != nil {
				err = errors.Join(err, e)
			}
		}
		rt.closeErr = err
	})
	return rt.closeErr
}

// Config returns the last loaded project config.
func (rt *Runtime) Config() *config.Settings {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return projectConfigFromSettings(rt.opts.settingsSnapshot)
}

// Settings exposes the merged settings.json snapshot for callers that need it.
func (rt *Runtime) Settings() *config.Settings {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return config.MergeSettings(nil, rt.opts.settingsSnapshot)
}

// UpdateSystemPrompt replaces the identity section of the system prompt.
// An empty text removes the identity section entirely. The change takes effect
// on the next Run/RunStream call — a request already in flight reads the prompt
// once at the start of its run loop and is unaffected by mid-run updates.
func (rt *Runtime) UpdateSystemPrompt(text string) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.opts.SystemPromptBuilder == nil {
		rt.opts.SystemPromptBuilder = NewSystemPromptBuilder()
	}
	rt.opts.SystemPromptBuilder.RemoveSection(SystemPromptSectionIdentity)
	if strings.TrimSpace(text) != "" {
		rt.opts.SystemPromptBuilder.AddSection(SystemPromptSectionIdentity, text, SystemPromptPriorityIdentity)
	}
	rt.opts.SystemPrompt = rt.opts.SystemPromptBuilder.Build()
}

// GetSystemPrompt returns the current fully-built system prompt.
func (rt *Runtime) GetSystemPrompt() string {
	if rt == nil {
		return ""
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.opts.SystemPrompt
}

// ReloadAgentsMD re-reads AGENTS.md from the project root and replaces the
// memory section of the system prompt. Takes effect on the next Run/RunStream.
func (rt *Runtime) ReloadAgentsMD() error {
	if rt == nil {
		return nil
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()

	memory, err := config.LoadAgentsMD(rt.opts.ProjectRoot, rt.opts.fsLayer)
	if err != nil {
		return fmt.Errorf("api: reload agents.md: %w", err)
	}

	if rt.opts.SystemPromptBuilder == nil {
		rt.opts.SystemPromptBuilder = NewSystemPromptBuilder()
	}
	rt.opts.SystemPromptBuilder.RemoveSection(SystemPromptSectionMemory)
	if strings.TrimSpace(memory) != "" {
		rt.opts.SystemPromptBuilder.AddSection(
			SystemPromptSectionMemory,
			fmt.Sprintf("## Memory\n\n%s", strings.TrimSpace(memory)),
			SystemPromptPriorityMemory,
		)
	}
	rt.opts.SystemPrompt = rt.opts.SystemPromptBuilder.Build()
	return nil
}

// UpdateSettings replaces the in-memory settings snapshot.
// The new snapshot is reflected immediately in Settings()/Config() getters
// and in the SandboxReport returned with each response.
// NOTE: fields like DisallowedTools and Env are read once during New()
// and are NOT re-evaluated at runtime — changing them via UpdateSettings
// does not affect already-registered tools or the sandbox manager.
// A restart is required for those fields to take effect.
func (rt *Runtime) UpdateSettings(s *config.Settings) {
	if rt == nil || s == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Defensive clone: align with Settings() which clones on read.
	rt.opts.settingsSnapshot = config.MergeSettings(nil, s)
}

// AddMCPServer dynamically registers an MCP server at runtime.
// The cfg.Type determines the connection mode: "stdio" (Command+Args),
// "sse" (URL), or "http" (URL).
func (rt *Runtime) AddMCPServer(ctx context.Context, name string, cfg config.MCPServerConfig) error {
	if rt == nil {
		return fmt.Errorf("runtime is nil")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()

	spec := mcpConfigToSpec(cfg)
	if strings.TrimSpace(spec) == "" {
		return fmt.Errorf("invalid MCP server config: empty spec")
	}

	opts := tool.MCPServerOptions{
		Headers:       cfg.Headers,
		Env:           cfg.Env,
		EnabledTools:  cfg.EnabledTools,
		DisabledTools: cfg.DisabledTools,
	}
	if cfg.TimeoutSeconds > 0 {
		opts.Timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	if cfg.ToolTimeoutSeconds > 0 {
		opts.ToolTimeout = time.Duration(cfg.ToolTimeoutSeconds) * time.Second
	}

	return rt.registry.RegisterMCPServerWithOptions(ctx, spec, name, opts)
}

// RemoveMCPServer disconnects and removes an MCP server by its serverID.
func (rt *Runtime) RemoveMCPServer(serverID string) error {
	if rt == nil {
		return fmt.Errorf("runtime is nil")
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.registry.RemoveMCPServer(serverID)
}

// ListMCPServers returns a snapshot of all registered MCP servers.
func (rt *Runtime) ListMCPServers() []tool.MCPServerInfo {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.registry.ListMCPServers()
}

func mcpConfigToSpec(cfg config.MCPServerConfig) string {
	cfg.Type = strings.ToLower(strings.TrimSpace(cfg.Type))
	switch cfg.Type {
	case "stdio":
		return fmt.Sprintf("stdio://%s %s", cfg.Command, strings.Join(cfg.Args, " "))
	case "sse":
		return cfg.URL
	case "http", "":
		// Add +stream hint so the SDK uses Streamable HTTP transport
		// instead of SSE for plain https:// URLs.
		return addStreamHint(cfg.URL)
	default:
		return cfg.URL
	}
}

// addStreamHint adds a +stream hint to http(s) URLs that don't already have one.
func addStreamHint(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return url
	}
	lowered := strings.ToLower(url)
	if strings.HasPrefix(lowered, "https://") {
		return "https+stream://" + url[8:]
	}
	if strings.HasPrefix(lowered, "http://") {
		return "http+stream://" + url[7:]
	}
	return url
}

// ReloadSkill re-reads and re-registers a single skill from the filesystem.
// This is used when a SKILL.md file has been created or modified at runtime.
func (rt *Runtime) ReloadSkill(name string) error {
	if rt == nil {
		return fmt.Errorf("runtime is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("skill name is empty")
	}

	skillDir := filepath.Join(rt.opts.ProjectRoot, ".agents", "skills", name)
	registration, err := skills.LoadSingleSkill(skillDir, rt.opts.fsLayer)
	if err != nil {
		return fmt.Errorf("api: reload skill %s: %w", name, err)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.opts.skReg == nil {
		return fmt.Errorf("skills registry is nil")
	}
	// Unregister first to allow re-registration (Register would fail on duplicate name)
	rt.opts.skReg.Unregister(name)
	if err := rt.opts.skReg.Register(registration.Definition, registration.Handler); err != nil {
		return fmt.Errorf("api: register skill %s: %w", name, err)
	}
	return nil
}

// UnregisterSkill removes a skill from the registry by name.
func (rt *Runtime) UnregisterSkill(name string) bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.opts.skReg == nil {
		return false
	}
	return rt.opts.skReg.Unregister(name)
}

// ListSkills returns all registered skill definitions.
func (rt *Runtime) ListSkills() []skills.Definition {
	if rt == nil {
		return nil
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.opts.skReg == nil {
		return nil
	}
	return rt.opts.skReg.List()
}

// Sandbox exposes the sandbox manager.
func (rt *Runtime) Sandbox() *sandbox.Manager {
	if rt == nil || rt.executor == nil {
		return nil
	}
	return rt.executor.Sandbox()
}

// SessionHistory returns a cloned snapshot of an existing session history.
func (rt *Runtime) SessionHistory(sessionID string) ([]message.Message, bool) {
	if rt == nil || rt.histories == nil {
		return nil, false
	}
	return rt.histories.Snapshot(sessionID)
}

// ClearSession 清空指定 session 的历史记录。
// 返回 true 表示 session 存在并被清空，false 表示 session 不存在。
func (rt *Runtime) ClearSession(sessionID string) bool {
	if rt == nil || rt.histories == nil {
		return false
	}
	return rt.histories.Clear(sessionID)
}

// ----------------- internal helpers -----------------
