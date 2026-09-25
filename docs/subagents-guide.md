# Subagents Guide: Fork-based Tool vs Native Routing

This SDK ships **two** subagent mechanisms that coexist with different responsibilities:

- **Built-in `subagent` tool** (in `pkg/api`, name `subagent`): the main agent (LLM) decides at runtime to dispatch a sub-agent, and the sub-agent forks the complete conversation context.
- **Native routing mechanism** (in `pkg/runtime/subagents`): the SDK dispatches a sub-agent *before* the main agent loop starts, based on an explicit request field or keyword matchers.

This guide compares the two, explains when to use which, and documents agent types, async dispatch, session resumption, and prompt-cache implications of the built-in tool.

## Two Mechanisms at a Glance

| Dimension | Built-in `subagent` tool | Native routing mechanism |
|---|---|---|
| Who decides | The main agent (LLM) emits `tool_use: subagent` | The SDK routes in `prepare()`, before the agent loop |
| Trigger | LLM judgement | `Request.TargetSubagent`; otherwise keyword matchers |
| Context | Deep copy of the full main-session history, subtask appended at the tail | Independent context from the definition + current prompt |
| Definitions | Agent types: `Options.AgentTypes` or `.agents/subagents/*.md` | `.agents/agents/` + `Options.Subagents`; built-ins `general-purpose`, `explore`, `plan` |
| Result flow | Returned as a tool result; the main agent decides how to use it | **Replaces** the prompt; the main loop then runs with it |
| Async | `background: true` tool parameter | `Manager.DispatchAsync` / `TaskStatus` |
| Completion delivery | Hooks `SubagentComplete` event | Hooks `SubagentComplete` event, optional `[Subagent Result: ...]` message appended to the session |
| Background concurrency | Managed by the caller (one goroutine per dispatch) | `Options.MaxConcurrentSubagents` (default 3) |
| Nested dispatch | Not possible (sub-runtime excludes `subagent`) | Handler-defined |

## Choosing Between Them

Use the **built-in `subagent` tool** when:

- The decision to delegate belongs to the model, not to your code (Claude Code-style behavior).
- The sub-agent needs the full conversation context — e.g. "review everything we discussed and produce a conclusion".
- The task is long-running and the conversation must stay responsive (`background: true`).
- You want follow-up questions to a sub-agent without re-forking the context (`sub_session_id`).

Use the **native routing mechanism** when:

- Dispatch is deterministic and driven by your application (no LLM judgement), via `Request.TargetSubagent` or keyword matchers.
- The sub-agent output *is* the real prompt for the main loop (pre-processing pipeline).
- You need fan-out over a team (`Request.TeamMembers`) or you already maintain `.agents/agents/` definitions.

Do not mix both on the same request: when `Request.TargetSubagent` matches, the native dispatch consumes the request before the agent loop starts, and the `subagent` tool never sees it. The two mechanisms are otherwise unaware of each other.

## Built-in `subagent` Tool

The tool is registered as the built-in `subagent` and is controlled like any other built-in (`Options.EnabledBuiltinTools` to select, `Options.DisallowedTools` to block). It is enabled by default.

When executed, it:

1. Deep-copies the main session history (via the runtime's `HistoryLoader` seam);
2. Appends one user message at the tail: `[Subtask] <name>: <instruction>` plus the output contract and (if the type has one) the type's append prompt;
3. Creates a sub-runtime with the same model, system prompt, tools, MCP servers, skills and rules, excluding the `subagent` tool itself;
4. Runs the sub-agent under an independent session id `<main-session-id>-sub-<uuid>`;

On success it returns `Success: true` with the sub-agent output as `Output` and the sub-session id in `Data.sub_session_id`. On failure it returns `Success: false` with the error text, so the main agent can react instead of silently losing the result.

### Parameters

| Parameter | Type | Description |
|---|---|---|
| `name` | string, required | Sub-agent name used for the subtask label |
| `instruction` | string, required | Self-contained task instruction |
| `type` | string | Agent type name; empty uses the default general type |
| `background` | bool | `true` returns immediately and notifies via hooks on completion; default `false` blocks |
| `sub_session_id` | string | Continue an existing sub-session instead of forking again |

## Agent Types

An agent type configures one named flavor of sub-agent. When no type is selected (or the name is not registered), the default general type inherits the main runtime configuration unchanged — this is the backward-compatible path.

```go
rt, err := api.New(ctx, api.Options{
    ProjectRoot: ".",
    ModelFactory: provider,
    AgentTypes: []api.AgentType{
        {
            Name:         "explorer",
            Description:  "Read-only code navigation and Q&A",
            AllowedTools: []string{"read", "glob", "grep"},
        },
        {
            Name:         "auditor",
            Description:  "Security review against OWASP top 10",
            AppendPrompt: "Report each finding with file:line and severity.",
        },
    },
})
```

- `AllowedTools` empty → the sub-agent inherits all tools of the main agent. Non-empty → narrows the tool set to the listed built-in and custom tools. MCP tools and skills injection are intentionally not filtered.
- `AppendPrompt` is appended to the tail fork message only. It never modifies the sub-runtime's system prompt, which keeps the request prefix identical to the main runtime.

### File-based Definitions

Definitions can also live in `<ProjectRoot>/.agents/subagents/*.md`, using YAML frontmatter (same style as skill files) plus the prompt body:

```markdown
---
name: explorer
description: Read-only code navigation and Q&A
allowed-tools: read, glob, grep
---

Answer questions about the codebase using search and reading only.
Report file:line references for every claim.
```

When a file-defined and a programmatically registered type share a name, the programmatic registration wins — explicit code configuration overrides leftover project files. Registered types are listed in the tool description so the main agent can match by task nature.

## Async Dispatch

With `background: true`, `Execute` returns immediately:

- `Output` states that the task was dispatched and names the task id;
- `Data.task_id` / `Data.sub_session_id` carry the sub-session id; `Data.async` is `true`.

Completion and failure are both delivered on the hooks bus as `SubagentComplete` events with payload fields `TaskID`, `Name`, `Status` (`success`/`error`), `Output` (truncated to 2000 chars), `Error`, `AgentType`, `Duration`, and `OutputLength` (the untruncated length). This fits IM front-ends that push a card when the event arrives. Async tasks run under a context bound to the main runtime: `Runtime.Close()` cancels them and waits, so no goroutine outlives the runtime.

## Session Resumption

Each successful dispatch returns a sub-session id (`Data.sub_session_id`). Passing that id back via the `sub_session_id` parameter continues the sub-agent with its full working memory instead of re-forking the main context:

- The id must belong to the current main session: it has to start with `<main-session-id>-sub-`, otherwise the request is rejected.
- The id must exist in the in-memory cache; a type argument that disagrees with the first dispatch's type is rejected, so the tool whitelist and append prompt cannot drift.
- The main session history is never modified by follow-ups.
- The cache is process-local (no file persistence) and lifetime-bound: `Runtime.ClearSubagentSession(id)` drops one entry explicitly, `Runtime.Close()` releases all of them.

## Prompt-Cache Considerations

The sub-runtime keeps the same system prompt, model, tools, MCP servers and skills as the main runtime; differentiation is confined to the tail fork message so the cached prefix survives.

- **Default type** (no whitelist, no append prompt): the request prefix differs from the main runtime only in the tools array, which excludes the `subagent` tool.
- **Whitelist types** change the tools array, so the prefix diverges earlier.
- **Append-prompt-only types** keep the tools array identical to the default; the divergence is confined to the last forked message — the cache-friendliest differentiated form.

Guidance: prefer append-prompt types for behavioral differences; reserve tool whitelists for tasks where a narrower tool surface is genuinely required (e.g. read-only roles) and accept the prefix miss. Cache-hit numbers are provider-dependent — only the Anthropic provider fills `Usage.CacheReadTokens`/`CacheCreationTokens` — so measure before optimizing. A reproducible harness lives in `test/integration/subagent_kvcache_test.go` (build tag `integration`, requires `ANTHROPIC_API_KEY`); the methodology, prefix analysis and decision framework are recorded in `docs/spec/2026-09-24-subagent-agent-types-and-async.md` (Further Notes).

## Notes

- Sub-agents share the main process filesystem and external services; tool calls made by a sub-agent have real side effects.
- A sub-agent can not dispatch further sub-agents; the `subagent` tool is excluded from the sub-runtime.
- Sub-session history is in-memory only; it is not written to the user's session files.
- Both mechanisms publish `SubagentComplete`, but only the built-in tool populates `AgentType`, `Duration` and `OutputLength`.
