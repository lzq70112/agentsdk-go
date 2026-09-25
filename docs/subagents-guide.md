# Subagents Guide: Built-in Tool vs Native Routing

This SDK ships **two** subagent mechanisms that coexist with different responsibilities:

- **Built-in `subagent` tool** (in `pkg/api`, name `subagent`): the main agent (LLM) decides at runtime to dispatch a sub-agent. The sub-agent runs in an **isolated context** — it does not see the main conversation history and runs under its own identity system prompt.
- **Native routing mechanism** (in `pkg/runtime/subagents`): the SDK dispatches a sub-agent *before* the main agent loop starts, based on an explicit request field or keyword matchers.

This guide compares the two, explains when to use which, and documents agent types, async dispatch, session resumption, and isolation implications of the built-in tool.

## Two Mechanisms at a Glance

| Dimension | Built-in `subagent` tool | Native routing mechanism |
|---|---|---|
| Who decides | The main agent (LLM) emits `tool_use: subagent` | The SDK routes in `prepare()`, before the agent loop |
| Trigger | LLM judgement | `Request.TargetSubagent`; otherwise keyword matchers |
| Context | Isolated: no main-session history; starts from one `[Subtask]` delegation message | Independent context from the definition + current prompt |
| Identity | Sub-agent identity system prompt (type-specific, else a default); does **not** inherit the main agent's identity prompt | Defined by the agent definition |
| Inherited from main | Project rules / `AGENTS.md` memory, model, tools, MCP servers, skills | Definition + project context |
| Definitions | Agent types: `Options.AgentTypes` or `.agents/subagents/*.md` | `.agents/agents/` + `Options.Subagents`; built-ins `general-purpose`, `explore`, `plan` |
| Result flow | Returned as a tool result; the main agent decides how to use it | **Replaces** the prompt; the main loop then runs with it |
| Async | `background: true` tool parameter | `Manager.DispatchAsync` / `TaskStatus` |
| Completion delivery | Hooks `SubagentComplete` event | Hooks `SubagentComplete` event, optional `[Subagent Result: ...]` message appended to the session |
| Background concurrency | `Options.MaxConcurrentSubagents` (default 3) | `Options.MaxConcurrentSubagents` (default 3) |
| Nested dispatch | Hard-blocked at 1 layer: the sub-runtime keeps the `subagent` tool but it is a disabled no-op | Handler-defined |

## Choosing Between Them

Use the **built-in `subagent` tool** when:

- The decision to delegate belongs to the model, not to your code (Claude Code-style behavior).
- The task is self-contained and describable in an instruction — the sub-agent has no access to the main conversation, so it works from the instruction alone.
- You want a fresh perspective uncontaminated by the main conversation.
- The task is long-running and the conversation must stay responsive (`background: true`).
- You want follow-up questions to a sub-agent without restating the context (`sub_session_id`).

Use the **native routing mechanism** when:

- Dispatch is deterministic and driven by your application (no LLM judgement), via `Request.TargetSubagent` or keyword matchers.
- The sub-agent output *is* the real prompt for the main loop (pre-processing pipeline).
- You need fan-out over a team (`Request.TeamMembers`) or you already maintain `.agents/agents/` definitions.

Do not mix both on the same request: when `Request.TargetSubagent` matches, the native dispatch consumes the request before the agent loop starts, and the `subagent` tool never sees it. The two mechanisms are otherwise unaware of each other.

## Built-in `subagent` Tool

The tool is registered as the built-in `subagent` and is controlled like any other built-in (`Options.EnabledBuiltinTools` to select, `Options.DisallowedTools` to block). It is enabled by default.

When executed, it:

1. Builds an isolated context: the sub-session history starts empty (or from the cached sub-session history on a follow-up), not from the main session history;
2. Sends one delegation message: `[Subtask] <name>: <instruction>` plus the output contract;
3. Creates a sub-runtime with the same model, tools, MCP servers, skills and rules, but with the **identity system prompt replaced** by the type's `SystemPrompt` (or a default sub-agent identity when the type has none); project rules / `AGENTS.md` memory are still inherited;
4. Runs the sub-agent under an independent session id `<main-session-id>-sub-<uuid>`;

On success it returns `Success: true` with the sub-agent output as `Output` and the sub-session id in `Data.sub_session_id`. On failure it returns `Success: false` with the error text, so the main agent can react instead of silently losing the result.

### Parameters

| Parameter | Type | Description |
|---|---|---|
| `name` | string, required | Sub-agent name used for the subtask label |
| `instruction` | string, required | Self-contained task instruction |
| `type` | string | Agent type name; empty uses the default general type |
| `background` | bool | `true` returns immediately and notifies via hooks on completion; default `false` blocks |
| `sub_session_id` | string | Continue an existing sub-session instead of starting a fresh one |

## Agent Types

An agent type configures one named flavor of sub-agent. When no type is selected (or the name is not registered), the default general type uses the default sub-agent identity prompt.

```go
rt, err := api.New(ctx, api.Options{
    ProjectRoot: ".",
    ModelFactory: provider,
    AgentTypes: []api.AgentType{
        {
            Name:         "explorer",
            Description:  "Read-only code navigation and Q&A",
            SystemPrompt: "You are a read-only code explorer. Report file:line for every claim.",
        },
        {
            Name:         "auditor",
            Description:  "Security review against OWASP top 10",
            SystemPrompt: "You are a security auditor. Report each finding with file:line and severity.",
        },
    },
})
```

- `SystemPrompt` becomes the sub-runtime's identity system prompt. It replaces the main agent's identity section; project rules / `AGENTS.md` memory are still inherited. When empty, a built-in default sub-agent identity is used.
- `AllowedTools` is retained for compatibility but **does not narrow** the sub-agent's tool set: the sub-runtime keeps the main runtime's full tool surface (including `subagent` itself, which is a disabled no-op there).

### File-based Definitions

Definitions can also live in `<ProjectRoot>/.agents/subagents/*.md`, using YAML frontmatter (same style as skill files) plus the identity system prompt body:

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

Each successful dispatch returns a sub-session id (`Data.sub_session_id`). Passing that id back via the `sub_session_id` parameter continues the sub-agent with its full working memory instead of starting a fresh isolated context:

- The id must belong to the current main session: it has to start with `<main-session-id>-sub-`, otherwise the request is rejected.
- The id must exist in the in-memory cache; a type argument that disagrees with the first dispatch's type is rejected, so the identity system prompt cannot drift.
- The main session history is never modified by follow-ups.
- The cache is process-local (no file persistence) and lifetime-bound: `Runtime.ClearSubagentSession(id)` drops one entry explicitly, `Runtime.Close()` releases all of them.

## Isolation and Prompt Cache

The sub-runtime inherits the model, tools, MCP servers and skills of the main runtime, but it is **not** a prefix of the main request: it has its own identity system prompt and its own (isolated) message history. Consequently the main conversation's cached prefix is not reused by the sub-agent — cache behavior is measured only against repeated identical sub-agent requests, not against the parent prefix.

## Notes

- Sub-agents share the main process filesystem and external services; tool calls made by a sub-agent have real side effects.
- A sub-agent can not dispatch further sub-agents: the `subagent` tool is present in the sub-runtime but is a disabled no-op that refuses the call (max 1 layer).
- Sub-session history is in-memory only; it is not written to the user's session files.
- Both mechanisms publish `SubagentComplete`, but only the built-in tool populates `AgentType`, `Duration` and `OutputLength`.
