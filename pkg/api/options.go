package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/lzq70112/agentsdk-go/pkg/config"
	hooks "github.com/lzq70112/agentsdk-go/pkg/hooks"
	"github.com/lzq70112/agentsdk-go/pkg/message"
	"github.com/lzq70112/agentsdk-go/pkg/middleware"
	"github.com/lzq70112/agentsdk-go/pkg/model"
	"github.com/lzq70112/agentsdk-go/pkg/plugin"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/skills"
	"github.com/lzq70112/agentsdk-go/pkg/runtime/subagents"
	"github.com/lzq70112/agentsdk-go/pkg/sandbox"
	"github.com/lzq70112/agentsdk-go/pkg/tool"
)

var (
	ErrMissingModel        = errors.New("api: model factory is required")
	ErrConcurrentExecution = errors.New("concurrent execution on same session is not allowed")
	ErrRuntimeClosed       = errors.New("api: runtime is closed")
	ErrMaxIterations       = errors.New("max iterations reached")
	ErrToolUseDenied       = errors.New("api: tool use denied by hook")
)

var resolveProjectRoot = ResolveProjectRoot

var filepathAbs = filepath.Abs

type EntryPoint string

const (
	EntryPointCLI               EntryPoint = "cli"
	EntryPointCI                EntryPoint = "ci"
	EntryPointPlatform          EntryPoint = "platform"
	defaultEntrypoint                      = EntryPointCLI
	defaultMaxSessions                     = 1000
	defaultStopReinjectionLimit            = 3
	defaultStreamStallTimeout              = 60 * time.Second
	defaultEscalationAttempts              = 3
	defaultEscalationCeiling               = 65536
)

type ModelTier string

const (
	ModelTierLow  ModelTier = "low"  // Low cost: Haiku
	ModelTierMid  ModelTier = "mid"  // Mid cost: Sonnet
	ModelTierHigh ModelTier = "high" // High cost: Opus
)

type ModeContext struct {
	EntryPoint EntryPoint
}

type SandboxOptions struct {
	Root          string
	AllowedPaths  []string
	NetworkAllow  []string
	ResourceLimit sandbox.ResourceLimits
}

type StreamStallConfig struct {
	Timeout         time.Duration `json:"timeout"`
	FallbackEnabled bool          `json:"fallback_enabled"`
}

// MemoryOptions 配置 auto memory 子系统（spec docs/spec/2026-10-31-auto-memory.md）。
type MemoryOptions struct {
	// Enabled 三态开关：nil（默认）= 启用；显式 false = 整链路禁用（常驻提示、
	// 召回、写守卫、沙箱放行均无痕）。bool 无法区分"未设置"与"显式禁用"，
	// 沿用 RulesEnabled 的指针三态先例。
	Enabled *bool
	// Dir 记忆目录；空 = ~/.agents/memory/<bucket>（bucket 由 ProjectRoot
	// 绝对路径规范化派生）。非空时整体覆盖默认派生。
	Dir string
	// ExtraDirs 只读召回源（宿主挂载的用户手写知识目录）：只参与召回，
	// 永不附过期警示，写入一律拒绝。
	ExtraDirs []string
	// SelectorTier 是二期 LLM 选择器的模型档位；一期 keyword 选择器不使用。
	SelectorTier ModelTier

	// PerSession 开启会话级记忆隔离：auto 记忆桶按会话细分（<Dir>/sessions/
	// <sanitize(sessionID)>/），召回只扫当前会话的桶、抽取与游标也落在该桶内，
	// 多用户 IM 场景下不同会话的记忆互不串扰；extraDirs 仍是跨会话共享的召回源。
	// 常驻 memory-usage 提示在 PerSession 下不注入——主对话不再被指引主动写
	// 记忆，记忆统一由回合后的抽取链路维护（"用户明确要求记住"由抽取兜底）。
	PerSession bool

	// autoDerived 标记本配置由 withDefaults 从 nil 派生（宿主未配置 Memory）。
	// 默认派生配置只保留一期语义（常驻提示/召回/写守卫），后台抽取不启用：
	// 抽取有子代理模型调用与记忆目录写盘副作用，不应在宿主从未提及 Memory
	// 时静默发生（spec §8 偏离点，见 maybeStartMemoryExtraction）。
	autoDerived bool
}

// enabled 报告 memory 是否启用：nil 配置 / nil 开关 / 显式 true 均视为启用。
func (m *MemoryOptions) enabled() bool {
	return m == nil || m.Enabled == nil || *m.Enabled
}

func (m *MemoryOptions) withDefaults(projectRoot string) *MemoryOptions {
	var cfg MemoryOptions
	if m != nil {
		cfg = *m
	}
	if cfg.SelectorTier == "" {
		cfg.SelectorTier = ModelTierLow
	}
	if !cfg.enabled() {
		return &cfg
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		// ProjectRoot 为空时 memory 视为禁用（没有可靠的桶名可派生）。
		if strings.TrimSpace(projectRoot) == "" {
			cfg.Enabled = boolValuePtr(false)
			return &cfg
		}
		home, ok := memoryHomeDir()
		if !ok {
			cfg.Enabled = boolValuePtr(false)
			return &cfg
		}
		dir = defaultMemoryDir(home, projectRoot)
	}
	if abs, err := filepathAbs(dir); err == nil {
		dir = abs
	}
	cfg.Dir = dir
	return &cfg
}

func (c StreamStallConfig) withDefaults() StreamStallConfig {
	cfg := c
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultStreamStallTimeout
	}
	if !cfg.FallbackEnabled && c.Timeout <= 0 {
		cfg.FallbackEnabled = true
	}
	return cfg
}

type MaxTokensEscalationConfig struct {
	Enabled     bool `json:"enabled"`
	MaxAttempts int  `json:"max_attempts"`
	Ceiling     int  `json:"ceiling"`
}

func (c MaxTokensEscalationConfig) withDefaults() MaxTokensEscalationConfig {
	cfg := c
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultEscalationAttempts
	}
	if cfg.Ceiling <= 0 {
		cfg.Ceiling = defaultEscalationCeiling
	}
	if !cfg.Enabled && c.MaxAttempts <= 0 && c.Ceiling <= 0 {
		cfg.Enabled = true
	}
	return cfg
}

type SkillRegistration struct {
	Definition skills.Definition
	Handler    skills.Handler
}

type SubagentRegistration struct {
	Definition subagents.Definition
	Handler    subagents.Handler
}

// SubagentCompletion 描述一次子 agent 完成事件。History 是该子会话自身的隔离历史
// 快照（不含主会话历史），宿主可据此在 SDK 之外持久化子会话，便于排查问题。
type SubagentCompletion struct {
	MainSessionID string
	SubSessionID  string
	Name          string
	AgentType     string
	History       []message.Message
	Err           error
}

type ModelFactory interface {
	Model(ctx context.Context) (model.Model, error)
}

type ModelFactoryFunc func(context.Context) (model.Model, error)

func (fn ModelFactoryFunc) Model(ctx context.Context) (model.Model, error) {
	if fn == nil {
		return nil, ErrMissingModel
	}
	return fn(ctx)
}

// Options configures the unified SDK runtime.
type Options struct {
	EntryPoint        EntryPoint
	Mode              ModeContext
	ProjectRoot       string
	EmbedFS           fs.FS
	SettingsPath      string
	SettingsOverrides *config.Settings
	SettingsLoader    *config.SettingsLoader

	Model        model.Model
	ModelFactory ModelFactory

	ModelPool            map[ModelTier]model.Model
	SubagentModelMapping map[string]ModelTier

	DefaultEnableCache bool

	SystemPrompt        string
	SystemPromptBuilder *SystemPromptBuilder
	RulesEnabled        *bool // nil = 默认启用，false = 禁用

	Middleware             []middleware.Middleware
	MiddlewareTimeout      time.Duration
	MaxIterations          int
	Timeout                time.Duration
	TokenLimit             int
	TokenBudget            TokenBudgetConfig
	MaxToolOutputSize      int
	ToolConcurrency        int
	StopReinjectionLimit   int
	StreamStall            StreamStallConfig
	MaxTokensEscalation    MaxTokensEscalationConfig
	MaxSessions            int
	MaxConcurrentSubagents int
	HistoryLoader          func(sessionID string) ([]message.Message, error) // restores persisted session history
	Tools                  []tool.Tool
	EnabledBuiltinTools    []string
	DisallowedTools        []string
	CustomTools            []tool.Tool
	MCPServers             []string

	// DisabledSkills 列出不注册的 skill 名（大小写不敏感）：New() 构建注册表时
	// 跳过，并随 buildSubOptions 继承给子 runtime，保证宿主的禁用决定对主/子
	// agent 一致生效。运行时可通过 Runtime.SetDisabledSkills 调整。
	DisabledSkills []string
	// DisabledMCP 列出不连接的 MCP server 配置名（settings.json mcp.servers 的键，
	// 大小写不敏感）：collectMCPServers 时跳过，并随 buildSubOptions 继承给子
	// runtime。运行时可通过 Runtime.SetDisabledMCP 调整。
	DisabledMCP []string

	// PluginsDir 指向插件注册表根目录（installed_plugins.json 所在）：非空则
	// New() 时经 plugin.Discover 自动发现已安装且启用的插件，四类资源
	// （skills/agents/commands/MCP）并入运行时；空串=不自动发现。宿主典型值
	// ~/.agents/plugins。变更经宿主重启进程生效。
	PluginsDir string
	// Plugins 宿主手动注入的插件（自管加载时机），与 PluginsDir 的发现结果合并，
	// 插件名冲突时手动注入优先。注入后视为只读，由 SDK 在 New() 时消费。
	Plugins []plugin.Plugin
	// DisabledPlugins 列出整体不加载的插件名（大小写不敏感；禁用粒度是整个
	// 插件而非单资源），随 buildSubOptions 经 BaseOptions 继承给子 runtime。
	DisabledPlugins []string

	TypedHooks             []hooks.ShellHook
	HookMiddleware         []hooks.Middleware
	HookTimeout            time.Duration
	DisableSafetyHook      bool
	DisableSubagentSummary bool

	// SubagentCompletionHandler 在每次子 agent 完成时被调用（同步派发与后台任务都会触发），
	// 携带该子会话的隔离历史快照，供宿主在 SDK 之外落盘或观测。为 nil 时不回调。
	SubagentCompletionHandler func(SubagentCompletion)

	// ScheduledFireHandler 在 cron 任务到期并成功注入会话后被调用，参数是推进后
	// 的任务快照（recurring 含新 NextRun；one-shot 含终态 LastRun）。注入只落
	// history 不驱动模型运行，宿主可借此发起唤醒回合让到期任务被真正执行。
	// 为 nil 时不回调。
	ScheduledFireHandler func(CronTask)

	Skills     []SkillRegistration
	Subagents  []SubagentRegistration
	AgentTypes []AgentType
	Sandbox    SandboxOptions
	// Memory 配置 auto memory 子系统；nil = 默认启用并派生默认目录（仅一期
	// 语义：常驻提示/召回/写守卫；后台抽取需宿主显式配置 Memory 才启用）。
	// 见 MemoryOptions 与 docs/spec/2026-10-31-auto-memory.md。
	Memory           *MemoryOptions
	AutoCompact      CompactConfig
	OTEL             OTELConfig
	LogDir           string // directory for runtime logs; empty disables file logging (typical: <ProjectRoot>/.agents/logs)
	Verbose          bool   // when true, routine logs are also mirrored to stderr
	fsLayer          *config.FS
	settingsSnapshot *config.Settings
	skReg            *skills.Registry
	subMgr           *subagents.Manager
	tracer           Tracer
	// permissionDeny 是 New() 时从 settings.Permissions.Deny 编译出的 deny
	// 规则（见 permission_deny.go），由 runtimeToolExecutor 在工具执行前拦截。
	permissionDeny []denyRule
	// plugins 是 New() 时 resolvePlugins 的解析结果（手动注入 + 磁盘发现，
	// 已应用 DisabledPlugins），供 skills/agents/MCP/commands 四路合并消费。
	plugins []plugin.Plugin
	// subagentDisabled 是 SDK 内部字段：为 true 时该 runtime 内的 subagent tool
	// 退化为"调用即拒绝"的 no-op（Name/Description/Schema 不变以命中 prefix cache），
	// 用于在子 runtime 中硬阻断孙 agent 派生。仅 buildSubOptions 会设置它。
	subagentDisabled bool
	// memoryExtractGuard 是抽取子代理专用的严格写守卫（spec §7.1）：write/edit
	// 只允许落记忆目录内的主题 .md。仅 memoryExtractor.buildSubOptions 会设置。
	memoryExtractGuard *memoryWriteGuard
}

func DefaultSubagentDefinitions() []subagents.Definition {
	return subagents.BuiltinDefinitions()
}

type Request struct {
	Prompt            string
	ContentBlocks     []model.ContentBlock // Multimodal content; when non-empty, used alongside Prompt
	Mode              ModeContext
	SessionID         string
	RequestID         string    `json:"request_id,omitempty"` // Auto-generated UUID or user-provided
	Model             ModelTier // Optional: override model tier for this request
	EnablePromptCache *bool     // Optional: enable prompt caching (nil uses global default)
	Traits            []string
	Tags              map[string]string
	Channels          []string
	Metadata          map[string]any
	// UserMetadata 随本回合的 user 消息写入会话历史 Metadata（与时间戳等
	// 框架键共存，键冲突时以宿主为准），随历史序列化自动落盘；不参与子任务
	// dispatch 元数据（那是 Metadata 的用途）。
	UserMetadata       map[string]any
	TeamMembers        []subagents.TeamMember
	TeamMaxAgents      int
	TeamMaxConcurrency int
	TargetSubagent     string
	ToolWhitelist      []string
	ForceSkills        []string
}

type Response struct {
	Mode            ModeContext
	RequestID       string `json:"request_id,omitempty"` // UUID for distributed tracing
	Result          *Result
	SkillResults    []SkillExecution
	Team            *subagents.TeamResult
	Subagent        *subagents.Result
	HookEvents      []hooks.Event
	ProjectConfig   *config.Settings
	Settings        *config.Settings
	SandboxSnapshot SandboxReport
	Tags            map[string]string
}

type Result struct {
	Output     string
	StopReason string
	Usage      model.Usage
	ToolCalls  []model.ToolCall
}

type SkillExecution struct {
	Definition skills.Definition
	Result     skills.Result
	Err        error
}

type SandboxReport struct {
	Roots          []string
	AllowedPaths   []string
	AllowedDomains []string
	ResourceLimits sandbox.ResourceLimits
}

// WithHistoryLoader sets a function that is called when a new session is created.
// The loader receives the session ID and should return previously persisted messages,
// or nil, nil if no history exists.
func WithHistoryLoader(loader func(string) ([]message.Message, error)) func(*Options) {
	return func(o *Options) { o.HistoryLoader = loader }
}

func WithMaxSessions(n int) func(*Options) {
	return func(o *Options) {
		if n > 0 {
			o.MaxSessions = n
		}
	}
}

func WithAutoCompact(config CompactConfig) func(*Options) {
	return func(o *Options) {
		o.AutoCompact = config
	}
}

func WithLogDir(dir string) func(*Options) {
	return func(o *Options) {
		o.LogDir = dir
	}
}

func WithVerbose(verbose bool) func(*Options) {
	return func(o *Options) {
		o.Verbose = verbose
	}
}

func WithOTEL(config OTELConfig) func(*Options) {
	return func(o *Options) {
		o.OTEL = config
	}
}

// WithMemoryOptions 配置 auto memory 子系统。Enabled 字段为三态（nil=启用），
// 需要禁用时用 WithMemoryDisabled 或显式 Enabled: boolPtr(false)。
func WithMemoryOptions(cfg MemoryOptions) func(*Options) {
	return func(o *Options) {
		clone := cfg
		o.Memory = &clone
	}
}

// WithMemoryDisabled 关闭 auto memory：整链路无痕（不注入常驻提示、不召回、
// 不拦写、不放宽沙箱）。
func WithMemoryDisabled() func(*Options) {
	return func(o *Options) {
		cfg := MemoryOptions{}
		if o.Memory != nil {
			cfg = *o.Memory
		}
		enabled := false
		cfg.Enabled = &enabled
		o.Memory = &cfg
	}
}

func (o Options) withDefaults() Options {
	if o.EntryPoint == "" {
		o.EntryPoint = defaultEntrypoint
	}
	if o.Mode.EntryPoint == "" {
		o.Mode.EntryPoint = o.EntryPoint
	}

	if o.ProjectRoot == "" || o.ProjectRoot == "." {
		if resolved, err := resolveProjectRoot(); err == nil {
			o.ProjectRoot = resolved
		} else {
			o.ProjectRoot = "."
		}
	}
	o.ProjectRoot = filepath.Clean(o.ProjectRoot)
	if trimmed := strings.TrimSpace(o.SettingsPath); trimmed != "" {
		if abs, err := filepathAbs(trimmed); err == nil {
			o.SettingsPath = abs
		} else {
			o.SettingsPath = trimmed
		}
	}

	if o.Sandbox.Root == "" {
		o.Sandbox.Root = o.ProjectRoot
	}

	if len(o.Sandbox.NetworkAllow) == 0 {
		o.Sandbox.NetworkAllow = defaultNetworkAllowList(o.EntryPoint)
	}

	if o.MaxSessions <= 0 {
		o.MaxSessions = defaultMaxSessions
	}
	if o.StopReinjectionLimit <= 0 {
		o.StopReinjectionLimit = defaultStopReinjectionLimit
	}
	o.StreamStall = o.StreamStall.withDefaults()
	o.MaxTokensEscalation = o.MaxTokensEscalation.withDefaults()
	if o.Memory == nil {
		// 宿主未配置 Memory：派生默认目录保持一期"默认启用"语义，但标记
		// autoDerived——后台抽取不在默认派生配置上运行（见 MemoryOptions）。
		o.Memory = (&MemoryOptions{}).withDefaults(o.ProjectRoot)
		o.Memory.autoDerived = true
	} else {
		o.Memory = o.Memory.withDefaults(o.ProjectRoot)
	}
	return o
}

// frozen returns a defensive copy of Options so callers can safely reuse/mutate
// the original Options struct without racing against a live Runtime.
func (o Options) frozen() Options {
	o.Mode = ModeContext{EntryPoint: o.Mode.EntryPoint}
	if o.SystemPromptBuilder != nil {
		o.SystemPromptBuilder = o.SystemPromptBuilder.Clone()
	}

	if len(o.Middleware) > 0 {
		o.Middleware = append([]middleware.Middleware(nil), o.Middleware...)
	}
	if len(o.Tools) > 0 {
		o.Tools = append([]tool.Tool(nil), o.Tools...)
	}
	if len(o.EnabledBuiltinTools) > 0 {
		o.EnabledBuiltinTools = append([]string(nil), o.EnabledBuiltinTools...)
	}
	if len(o.DisallowedTools) > 0 {
		o.DisallowedTools = append([]string(nil), o.DisallowedTools...)
	}
	if len(o.CustomTools) > 0 {
		o.CustomTools = append([]tool.Tool(nil), o.CustomTools...)
	}
	if len(o.MCPServers) > 0 {
		o.MCPServers = append([]string(nil), o.MCPServers...)
	}
	if len(o.DisabledSkills) > 0 {
		o.DisabledSkills = append([]string(nil), o.DisabledSkills...)
	}
	if len(o.DisabledMCP) > 0 {
		o.DisabledMCP = append([]string(nil), o.DisabledMCP...)
	}
	if len(o.DisabledPlugins) > 0 {
		o.DisabledPlugins = append([]string(nil), o.DisabledPlugins...)
	}
	if len(o.Plugins) > 0 {
		// 浅克隆防宿主后续 append/重排污染；条目内资源在 New() 合并时逐值复制。
		o.Plugins = append([]plugin.Plugin(nil), o.Plugins...)
	}
	if len(o.TypedHooks) > 0 {
		hooks := make([]hooks.ShellHook, len(o.TypedHooks))
		for i, hook := range o.TypedHooks {
			hooks[i] = hook
			if len(hook.Env) > 0 {
				hooks[i].Env = maps.Clone(hook.Env)
			}
		}
		o.TypedHooks = hooks
	}
	if len(o.HookMiddleware) > 0 {
		o.HookMiddleware = append([]hooks.Middleware(nil), o.HookMiddleware...)
	}
	if len(o.Skills) > 0 {
		skillsCopy := make([]SkillRegistration, len(o.Skills))
		for i, reg := range o.Skills {
			skillsCopy[i] = reg
			def := reg.Definition
			if len(def.Metadata) > 0 {
				def.Metadata = maps.Clone(def.Metadata)
			}
			if len(def.Matchers) > 0 {
				def.Matchers = append([]skills.Matcher(nil), def.Matchers...)
			}
			skillsCopy[i].Definition = def
		}
		o.Skills = skillsCopy
	}
	if len(o.Subagents) > 0 {
		subCopy := make([]SubagentRegistration, len(o.Subagents))
		for i, reg := range o.Subagents {
			subCopy[i] = reg
			def := reg.Definition
			def.BaseContext = def.BaseContext.Clone()
			if len(def.Matchers) > 0 {
				def.Matchers = append([]skills.Matcher(nil), def.Matchers...)
			}
			subCopy[i].Definition = def
		}
		o.Subagents = subCopy
	}
	if len(o.AgentTypes) > 0 {
		typesCopy := make([]AgentType, len(o.AgentTypes))
		for i, at := range o.AgentTypes {
			typesCopy[i] = at
			if len(at.AllowedTools) > 0 {
				typesCopy[i].AllowedTools = append([]string(nil), at.AllowedTools...)
			}
		}
		o.AgentTypes = typesCopy
	}

	o.Sandbox = freezeSandboxOptions(o.Sandbox)

	if o.Memory != nil {
		memory := *o.Memory
		memory.ExtraDirs = append([]string(nil), o.Memory.ExtraDirs...)
		o.Memory = &memory
	}

	if len(o.ModelPool) > 0 {
		o.ModelPool = maps.Clone(o.ModelPool)
	}
	if len(o.SubagentModelMapping) > 0 {
		o.SubagentModelMapping = maps.Clone(o.SubagentModelMapping)
	}

	return o
}

func freezeSandboxOptions(in SandboxOptions) SandboxOptions {
	out := in
	if len(in.AllowedPaths) > 0 {
		out.AllowedPaths = append([]string(nil), in.AllowedPaths...)
	}
	if len(in.NetworkAllow) > 0 {
		out.NetworkAllow = append([]string(nil), in.NetworkAllow...)
	}
	return out
}

// defaultNetworkAllowList 默认允许本地网络；访问外网需显式配置
func defaultNetworkAllowList(_ EntryPoint) []string {
	return []string{
		"localhost",
		"127.0.0.1",
		"::1",       // IPv6 localhost
		"0.0.0.0",   // 本机所有接口
		"*.local",   // 本地域名
		"192.168.*", // 私有网段
		"10.*",      // 私有网段
		"172.16.*",  // 私有网段
	}
}

func (o Options) modeContext() ModeContext {
	mode := o.Mode
	if mode.EntryPoint == "" {
		mode.EntryPoint = o.EntryPoint
	}
	if mode.EntryPoint == "" {
		mode.EntryPoint = defaultEntrypoint
	}
	return mode
}

func (r Request) normalized(defaultMode ModeContext, fallbackSession string) Request {
	req := r
	if req.Mode.EntryPoint == "" {
		req.Mode.EntryPoint = defaultMode.EntryPoint
	}
	if req.SessionID == "" {
		req.SessionID = strings.TrimSpace(fallbackSession)
	}
	if req.Tags == nil {
		req.Tags = map[string]string{}
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	if len(req.ToolWhitelist) > 0 {
		req.ToolWhitelist = cloneStrings(req.ToolWhitelist)
	}
	if len(req.ContentBlocks) > 0 {
		req.ContentBlocks = append([]model.ContentBlock(nil), req.ContentBlocks...)
	}
	if len(req.TeamMembers) > 0 {
		req.TeamMembers = cloneTeamMembers(req.TeamMembers)
	}
	if len(req.Channels) > 0 {
		req.Channels = cloneStrings(req.Channels)
	}
	if len(req.Traits) > 0 {
		req.Traits = cloneStrings(req.Traits)
	}
	return req
}

func (r Request) activationContext(prompt string) skills.ActivationContext {
	ctx := skills.ActivationContext{Prompt: prompt}
	if len(r.Channels) > 0 {
		ctx.Channels = append([]string(nil), r.Channels...)
	}
	if len(r.Metadata) > 0 {
		if current := stringSlice(r.Metadata["api.current_paths"]); len(current) > 0 {
			ctx.CurrentPaths = current
		} else if current := stringSlice(r.Metadata["current_paths"]); len(current) > 0 {
			ctx.CurrentPaths = current
		}
	}
	if len(r.Traits) > 0 {
		ctx.Traits = append([]string(nil), r.Traits...)
	}
	if len(r.Tags) > 0 {
		ctx.Tags = maps.Clone(r.Tags)
	}
	if len(r.Metadata) > 0 {
		ctx.Metadata = maps.Clone(r.Metadata)
	}
	return ctx
}

func cloneStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	slices.Sort(out)
	return slices.Compact(out)
}

func cloneTeamMembers(in []subagents.TeamMember) []subagents.TeamMember {
	if len(in) == 0 {
		return nil
	}
	out := make([]subagents.TeamMember, len(in))
	for i, member := range in {
		out[i] = subagents.TeamMember{
			Name:        strings.TrimSpace(member.Name),
			Instruction: strings.TrimSpace(member.Instruction),
		}
		if len(member.Metadata) > 0 {
			out[i].Metadata = maps.Clone(member.Metadata)
		}
		if len(member.ToolWhitelist) > 0 {
			out[i].ToolWhitelist = cloneStrings(member.ToolWhitelist)
		}
	}
	return out
}

// WithModelPool configures a pool of models indexed by tier.
func WithModelPool(pool map[ModelTier]model.Model) func(*Options) {
	return func(o *Options) {
		if pool != nil {
			o.ModelPool = pool
		}
	}
}

// WithSubagentModelMapping configures subagent-type-to-tier mappings for model selection.
// Keys should be lowercase subagent type names (e.g., "explore", "plan").
func WithSubagentModelMapping(mapping map[string]ModelTier) func(*Options) {
	return func(o *Options) {
		if mapping != nil {
			o.SubagentModelMapping = mapping
		}
	}
}

// HookRecorder mirrors the historical api hook recorder contract.
type HookRecorder interface {
	Record(hooks.Event)
	Drain() []hooks.Event
}

// hookRecorder stores hook events for the response payload.
type hookRecorder struct {
	events []hooks.Event
}

func (r *hookRecorder) Record(evt hooks.Event) {
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	r.events = append(r.events, evt)
}

func (r *hookRecorder) Drain() []hooks.Event {
	defer func() { r.events = nil }()
	if len(r.events) == 0 {
		return nil
	}
	return append([]hooks.Event(nil), r.events...)
}

// defaultHookRecorder implements HookRecorder when callers do not provide one.
func defaultHookRecorder() *hookRecorder {
	return &hookRecorder{}
}

// runtimeHookAdapter wraps the hook executor and recorder.
type runtimeHookAdapter struct {
	executor *hooks.Executor
	recorder HookRecorder

	disableSafetyHook bool
	// memoryGuard 是 auto memory 的内部写守卫（Memory 禁用时为 nil）：
	// 在 SafetyCheck 之后、用户 hook 之前拦截记忆目录的写操作。
	memoryGuard *memoryWriteGuard
}

func (h *runtimeHookAdapter) PreToolUse(ctx context.Context, evt hooks.ToolUsePayload) (map[string]any, error) {
	params := evt.Params
	if h == nil {
		if err := hooks.SafetyCheck(evt.Name, params); err != nil {
			return nil, err
		}
		return params, nil
	}
	if !h.disableSafetyHook {
		if err := hooks.SafetyCheck(evt.Name, params); err != nil {
			return nil, err
		}
	}
	// 记忆写守卫：命中即 deny（错误经 ErrToolUseDenied 包装），不进用户 hook。
	if err := h.memoryGuard.preToolUse(evt.Name, params); err != nil {
		return nil, err
	}
	if h.executor == nil {
		return params, nil
	}
	results, err := h.executor.Execute(ctx, hooks.Event{Type: hooks.PreToolUse, Payload: evt})
	if err != nil {
		return nil, err
	}
	h.record(hooks.Event{Type: hooks.PreToolUse, Payload: evt})

	// Print hook stderr output for debugging
	for _, res := range results {
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
	}

	for _, res := range results {
		if res.Output == nil {
			continue
		}
		// Check top-level decision
		if res.Output.Decision == "deny" {
			return nil, fmt.Errorf("%w: %s", ErrToolUseDenied, evt.Name)
		}
		// Check continue=false
		if res.Output.Continue != nil && !*res.Output.Continue {
			return nil, fmt.Errorf("%w: %s", ErrToolUseDenied, evt.Name)
		}
		// Check hookSpecificOutput for PreToolUse
		if hso := res.Output.HookSpecificOutput; hso != nil {
			if hso.UpdatedInput != nil {
				params = hso.UpdatedInput
			}
			if hso.PermissionDecision == "deny" {
				return nil, fmt.Errorf("%w: %s", ErrToolUseDenied, evt.Name)
			}
		}
	}
	return params, nil
}

func (h *runtimeHookAdapter) PostToolUse(ctx context.Context, evt hooks.ToolResultPayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	results, err := h.executor.Execute(ctx, hooks.Event{Type: hooks.PostToolUse, Payload: evt})
	if err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.PostToolUse, Payload: evt})

	// Print hook stderr output for debugging
	for _, res := range results {
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
	}

	// Check if any hook wants to stop
	for _, res := range results {
		if res.Output != nil && res.Output.Continue != nil && !*res.Output.Continue {
			return fmt.Errorf("hooks: PostToolUse hook requested stop: %s", res.Output.StopReason)
		}
	}
	return nil
}

func (h *runtimeHookAdapter) Stop(ctx context.Context, reason string) error {
	_, err := h.evaluateStop(ctx, reason)
	return err
}

func (h *runtimeHookAdapter) evaluateStop(ctx context.Context, reason string) (string, error) {
	if h == nil || h.executor == nil {
		return "", nil
	}
	payload := hooks.StopPayload{Reason: reason}
	results, err := h.executor.Execute(ctx, hooks.Event{Type: hooks.Stop, Payload: payload})
	if err != nil {
		return "", err
	}
	for _, res := range results {
		if res.Stderr != "" {
			fmt.Fprint(os.Stderr, res.Stderr)
		}
		if res.Output == nil {
			continue
		}
		if blockingReason, ok := stopBlockingReason(res.Output); ok {
			payload.BlockingError = blockingReason
			break
		}
	}
	h.record(hooks.Event{Type: hooks.Stop, Payload: payload})
	return payload.BlockingError, nil
}

func stopBlockingReason(out *hooks.HookOutput) (string, bool) {
	if out == nil {
		return "", false
	}
	if strings.EqualFold(strings.TrimSpace(out.Decision), "block") {
		if reason := strings.TrimSpace(out.Reason); reason != "" {
			return reason, true
		}
		if reason := strings.TrimSpace(out.StopReason); reason != "" {
			return reason, true
		}
		return "blocked by stop hook", true
	}
	if out.Continue != nil && !*out.Continue {
		if reason := strings.TrimSpace(out.StopReason); reason != "" {
			return reason, true
		}
		if reason := strings.TrimSpace(out.Reason); reason != "" {
			return reason, true
		}
		return "blocked by stop hook", true
	}
	return "", false
}

func (h *runtimeHookAdapter) SessionStart(ctx context.Context, evt hooks.SessionStartPayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	if err := h.executor.Publish(hooks.Event{Type: hooks.SessionStart, Payload: evt}); err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.SessionStart, Payload: evt})
	return nil
}

func (h *runtimeHookAdapter) SessionEnd(ctx context.Context, evt hooks.SessionEndPayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	if err := h.executor.Publish(hooks.Event{Type: hooks.SessionEnd, Payload: evt}); err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.SessionEnd, Payload: evt})
	return nil
}

func (h *runtimeHookAdapter) SubagentStart(ctx context.Context, evt hooks.SubagentStartPayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	if err := h.executor.Publish(hooks.Event{Type: hooks.SubagentStart, Payload: evt}); err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.SubagentStart, Payload: evt})
	return nil
}

func (h *runtimeHookAdapter) SubagentStop(ctx context.Context, evt hooks.SubagentStopPayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	if err := h.executor.Publish(hooks.Event{Type: hooks.SubagentStop, Payload: evt}); err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.SubagentStop, Payload: evt})
	return nil
}

func (h *runtimeHookAdapter) SubagentComplete(ctx context.Context, evt hooks.SubagentCompletePayload) error {
	if h == nil || h.executor == nil {
		return nil
	}
	if err := h.executor.Publish(hooks.Event{Type: hooks.SubagentComplete, Payload: evt}); err != nil {
		return err
	}
	h.record(hooks.Event{Type: hooks.SubagentComplete, Payload: evt})
	return nil
}

func (h *runtimeHookAdapter) record(evt hooks.Event) {
	if h == nil || h.recorder == nil {
		return
	}
	h.recorder.Record(evt)
}
