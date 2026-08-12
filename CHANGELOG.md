# Changelog

All notable changes to agentsdk-go will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [2.0.0] - 2025-03-20

### Breaking Changes
- **Module rename**: `github.com/cexll/agentsdk-go` → `github.com/stellarlinkco/agentsdk-go`
- **Package consolidation**: 24+ packages → ~11 packages (~34K → ~15-20K non-test lines)
- **Middleware**: 6 stages → 4 (`BeforeAgent`, `BeforeTool`, `AfterTool`, `AfterAgent`); removed `BeforeModel`/`AfterModel`
- **Tool names**: `file_read` → `read`, `file_write` → `write`, `file_edit` → `edit`
- **Config directory**: `.claude/` → `.agents/` throughout
- **Model provider**: `model.NewAnthropicProvider()` with functional options removed; use `&model.AnthropicProvider{ModelName: "..."}` struct literal
- **Response access**: `result.Output` → `resp.Result.Output`
- **Compaction**: Strip strategy (zero LLM calls) replaces summarization model

### Removed
- Packages: `pkg/agent`, `pkg/core/events`, `pkg/core/hooks`, `pkg/core/middleware`, `pkg/security`, `pkg/runtime/commands`, `pkg/runtime/tasks`, `pkg/prompts`, `pkg/acp`
- Tools: `web_fetch`, `web_search`, `bash_output`, `bash_status`, `kill_task`, `task_*`, `ask_user_question`, `slash_command`
- Events: `UserPromptSubmit`, `PermissionRequest`, `PostToolUseFailure`, `PreCompact`, `ContextCompacted`, `Notification`, `TokenUsage`, `ModelSelected`, `MCPToolsChanged`
- `RELEASE_NOTES.md` (consolidated into this file)

### Added
- **`pkg/hooks`**: Merged event bus from `pkg/core/events` + `pkg/core/hooks`; 7 events (`PreToolUse`, `PostToolUse`, `SessionStart`, `SessionEnd`, `Stop`, `SubagentStart`, `SubagentStop`)
- **Safety hook** (`pkg/hooks/safety.go`): Go-native `PreToolUse` check blocking catastrophic bash commands (YOLO default); disable via `DisableSafetyHook=true`
- **Agent loop inlined** into `pkg/api` (removed `pkg/agent`)
- **Subagent result injection**: Summary injected into parent conversation history
- **`pkg/gitignore`**: Gitignore matcher used by grep/glob built-ins
- Examples: `08-safety-hook`, `09-compaction`, `10-hooks`, `11-reasoning`, `12-multimodal`
- `AGENTS.md` for runtime memory (loaded at startup, `@include` support)
- `docs/refactor/UPGRADING-v2.md` migration guide

### Changed
- **Single `model.Model` interface**: merged `agent.Model` + `model.Model` into `model.Model` (`Complete`/`CompleteStream`)
- **Middleware API**: `middleware.Funcs{Identifier: ..., OnBeforeAgent: ...}` replaces old struct pattern
- **Streaming**: `RunStream` now returns `(<-chan StreamEvent, error)`; event types use typed constants (`api.EventContentBlockDelta`, etc.)
- **Config**: Settings loaded from `~/.agents/` and `./.agents/` (no legacy config-dir fallback)
- **OpenAI-compatible endpoints**: configurable via `ANTHROPIC_BASE_URL` env var

---

## [0.5.2] - 2025-12-26

### Fixed
- **Environment Variables Override APIKey (#30)**: Fixed bug where `ANTHROPIC_AUTH_TOKEN` / `ANTHROPIC_API_KEY` env vars would override explicitly configured `APIKey` in `AnthropicConfig`
- **CI Stability**: Downgraded `golangci-lint-action` from v6 to v4

---

## [0.5.1] - 2025-12-25

### Breaking Changes
- **Skill Name Validation**: Enforces lowercase alphanumeric + hyphens only (underscores removed)
- **Support File Structure**: `templates/` → `assets/`, added `references/`

### Added
- Extended `SkillMetadata` with `license`, `compatibility`, `metadata` fields
- `ToolList` type supporting YAML string and sequence formats
- 100% Agent Skills Specification compliance

---

## [0.5.0] - 2025-12-24

### Changed
- **Context Compression**: Updated to use Claude Code official compression prompt (`pkg/api/compact.go`)

---

## [0.4.0] - 2025-12-12

### Added
- **Hooks System Extension (#11)**: 4 new hook events — `PermissionRequest`, `SessionStart/End`, `SubagentStart/Stop`
- **Multi-model Support (#12)**: Subagent-level model binding via `api.ModelFactory`
- **Token Statistics (#13)**: `TokenStats` struct with automatic accumulation
- **Auto Compact (#14)**: Automatic context compression at configurable threshold
- **Async Bash (#15)**: `background` parameter for non-blocking bash execution
- **DisallowedTools (#15)**: Block specific tools at runtime
- **Rules Configuration (#16)**: `.agents/rules/` with markdown loading
- **OpenTelemetry (#17)**: Distributed tracing with span propagation
- **UUID Tracking (#17)**: Request-level UUID for observability

### Changed
- **BREAKING**: `api.Options.ModelFactory` changed to `func(ctx context.Context) model.Model`
- Hooks run as shell commands (stdin JSON) instead of in-process interfaces

---

## [0.3.0] - 2025-01-26

### Added
- Custom tool registration via `api.WithCustomTools()`
- Example 05: Custom tools demo

### Changed
- **BREAKING**: Configuration directory standardized to `.agents/`
- Default file mode changed from `0o644` to `0o600`

---

## [0.2.0] - 2025-01-15

### Added
- Initial public release
- Core agent loop with middleware system
- Built-in tools: bash, file operations, grep, glob
- MCP integration, config hot-reload, sandbox

---

## [0.1.0] - 2025-01-01

### Added
- Project initialization
- Basic agent architecture
