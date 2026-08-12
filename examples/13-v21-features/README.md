# 13-v21-features — v2.1 Feature Demo

Demonstrates all v2.1 Agent Execution Contract features in a single runnable example.

## Features Covered

| Feature | Flag | Description |
|---------|------|-------------|
| Token Budget + Diminishing Returns | `-feature token_budget` | Loop stops when output tokens drop below threshold for N consecutive iterations |
| Micro-Compaction | `-feature micro_compact` | Strips reasoning blocks, media, and truncates old tool results at zero LLM cost |
| Tool Concurrency Partitioning | `-feature tool_concurrency` | Read-only tools execute concurrently; write tools execute serially |
| Per-Tool Output Limit | `-feature output_limit` | Tool output truncated beyond configurable byte limit |
| Subagent Async Dispatch | `-feature subagent_async` | Background subagent execution with task status query |
| SystemPromptBuilder | `-feature prompt_builder` | Priority-ordered section assembly with add/remove/clone |
| Deferred Tools | `-feature deferred` | Tools excluded from initial request until activated via ToolSearch |
| Teams | `-feature teams` | Team fanout with explicit members and automatic team selection via matcher-based auto-selection |

## Usage

```bash
# Run all feature demos
go run ./examples/13-v21-features -all

# Run a specific feature
go run ./examples/13-v21-features -feature token_budget -token-budget 5000
go run ./examples/13-v21-features -feature micro_compact
go run ./examples/13-v21-features -feature tool_concurrency -concurrency 4
go run ./examples/13-v21-features -feature output_limit -max-tool-output 100
go run ./examples/13-v21-features -feature subagent_async
go run ./examples/13-v21-features -feature prompt_builder
go run ./examples/13-v21-features -feature deferred
go run ./examples/13-v21-features -feature teams

# Common options
-session-id string   session identifier (default "v21-demo")
-max-iterations int  max iterations (default 20)
-token-budget int    token budget limit (0=disabled)
-concurrency int     tool concurrency limit (0=NumCPU)
-max-tool-output int max tool output in bytes (0=disabled)
```

## No API Key Required

All demos use stub model implementations — no external API calls needed.
