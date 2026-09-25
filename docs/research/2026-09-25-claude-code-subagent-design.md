# Claude Code / Anthropic 官方对子 agent（subagent）的设计原理调研

**研究日期**: 2026-09-25
**研究问题**: Claude Code / Anthropic 官方如何设计子 agent 的上下文、身份、嵌套、结果回传与工具面？为什么这套设计天然不会出现"子 agent 以为自己是主 agent"？对 agentsdk-go 现有 fork 方案有何启示？

## 结论摘要

**Claude Code 的子 agent 默认拿到的是隔离的全新上下文：只有"它自己的系统提示词 + 主 agent 写的一条 delegation 消息 + CLAUDE.md / git status 等少量环境信息"，主会话的对话历史一条都不给它。** 子 agent 的身份靠**为该 agent 单独编写的系统提示词**建立（官方示例统一以 "You are a …" 开头），而不是复用主 agent 的系统提示词。把主会话历史整体 fork 给子 agent 的做法在 Claude 体系里只作为显式特例存在（`/subtask` / `subagent_type: "fork"`），官方文档明确说它 "drops the input isolation that subagents otherwise provide"（放弃了 subagent 本应提供的输入隔离）。

---

## 来源清单（均为 Anthropic 官方一手来源，2026-09-25 抓取原文）

| # | 来源 | URL |
|---|------|-----|
| S1 | Claude Code 官方文档：Subagents（原 Sub-agents 页） | https://code.claude.com/docs/en/sub-agents |
| S2 | Claude Agent SDK 官方文档：Subagents | https://code.claude.com/docs/en/agent-sdk/subagents （同内容: https://docs.claude.com/en/api/agent-sdk/subagents ） |
| S3 | Anthropic 工程博客：How we built our multi-agent research system（2025-06-13） | https://www.anthropic.com/engineering/multi-agent-research-system |
| S4 | Claude Code 官方文档：Tools reference（Agent tool behavior） | https://code.claude.com/docs/en/tools-reference |
| S5 | Claude Code 官方文档：Prompt caching（Subagents and the cache） | https://code.claude.com/docs/en/prompt-caching |
| S6 | anthropics/claude-code 官方仓库 CHANGELOG | https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md |

> 注：Claude Code 版本演进很快，官方文档含大量 "Before v2.1.xxx" 的版本注释；本文结论基于 2026-09-25 抓取的文档（约 v2.1.2xx 系列），后续版本可能变化。以下每条结论均标注来源编号与锚点。

---

## 一、上下文：隔离的全新上下文，不是 fork 主会话历史

**结论：默认是隔离的全新上下文；主 agent 的对话历史不会原样传给子 agent。fork 主会话是显式特例（fork subagent / `/subtask`），官方明确定位为"放弃隔离"的例外。**

官方原文（S1, #what-loads-at-startup）：

> "Each subagent starts with a fresh, isolated context window. It doesn't see your conversation history, the skills you've already invoked, or the files Claude has already read. Claude composes a delegation message that summarizes the task, and the subagent works from there. The exception is a fork, which inherits the parent conversation instead of starting fresh."

Agent SDK 文档表述更直接（S2, #what-subagents-inherit）：

> "Unless the subagent is a fork, its context window starts fresh, with no parent conversation, but isn't empty. **The only content you pass from parent to subagent is the Agent tool's prompt string**, so include any file paths, error messages, or decisions the subagent needs directly in that prompt."

S2 用一张表列出非 fork 子 agent 的上下文里**有什么 / 没有什么**：

| 子 agent 收到的 | 子 agent **收不到**的 |
|---|---|
| 自己的系统提示词（`AgentDefinition.prompt`）+ Agent tool 的 prompt 字符串 | **父会话的对话历史或工具结果** |
| 项目 CLAUDE.md（除非 `omitClaudeMd`） | 预加载的 skill 内容（除非写在 `skills` 字段） |
| 工具定义（继承父会话或 `tools` 子集） | **父会话的系统提示词** |

S1 进一步列出非 fork 子 agent 初始上下文的完整构成：

- **System prompt**：agent 自己的 prompt + Claude Code 追加的环境信息，"not the Claude Code system prompt"（不是 Claude Code 的系统提示词）；
- **Task message**：Claude 交接时写的 delegation prompt；
- **CLAUDE.md 文件**：各级 CLAUDE.md 层级（内置 Explore/Plan 跳过）；
- **Git status**：子 agent 启动时读的快照（Explore/Plan 跳过）；
- **Preloaded skills**：`skills` 字段指定的 skill 全文；
- **Sibling roster**：一条列出 `main` 及会话内其他命名 agent 的系统提醒（v2.1.206+）。

工具参考（S4, #agent-tool-behavior）一句话概括：

> "The Agent tool spawns a subagent in a separate context window. The subagent works through its task autonomously, then returns its result to the parent conversation. **The parent doesn't see the subagent's intermediate tool calls or outputs, only that final result.**"

**例外——fork（与本 SDK 方案最可比的官方形态）**（S1, #fork-the-current-conversation）：

> "A fork is a subagent that inherits the entire conversation so far instead of starting fresh. **This drops the input isolation that subagents otherwise provide: a fork sees the same system prompt, tools, model, and message history as the main session**, so you can hand it a side task without re-explaining the situation. The fork's own tool calls still stay out of your conversation and only its final result comes back."

fork 由 `/subtask` 命令或 Agent tool 的 `subagent_type: "fork"` 触发，v2.1.212+ 可用；交互式会话默认开启 "fork mode"（v2.1.232+），可用 `CLAUDE_CODE_FORK_SUBAGENT=0` 关闭；非交互 `-p` 和 Agent SDK 默认关闭。S6 对应 changelog 条目："Subagent forking is now on by default: a `subagent_type: "fork"` subagent inherits the full conversation and prompt cache…"。

---

## 二、身份：有独立的、带身份声明的系统提示词

**结论：子 agent 使用为它单独编写的系统提示词，不继承主 agent 的 Claude Code 系统提示词；官方全部示例的身份声明都以 "You are a …" 开头。**

S1（#write-subagent-files）：

> "The frontmatter defines the subagent's metadata and configuration. The body becomes the system prompt that guides the subagent's behavior. **Subagents receive only this system prompt plus basic environment details like the working directory, not the Claude Code system prompt.**"

S1（#what-loads-at-startup）复述同一机制：

> "**System prompt**: the agent's own prompt plus environment details that Claude Code appends, **not the Claude Code system prompt**. Custom subagents define theirs in the markdown body or `prompt` field. Built-in agents have predefined prompts."

S2 的 `AgentDefinition` 字段表：`prompt` = "The agent's system prompt defining its role and behavior"（必填）。

官方文档给出的 prompt 示例（S1 #example-subagents、#quickstart；S2 代码示例），身份句式高度一致：

- "You are a code improvement specialist. …"
- "You are a senior code reviewer ensuring high standards of code quality and security."
- "You are an expert debugger specializing in root cause analysis."
- "You are a data scientist specializing in SQL and BigQuery analysis."
- "You are a code review specialist with expertise in security, performance, and best practices."

除提示词外，官方还有两个**结构性**的身份/边界机制（不是靠模型自觉）：

1. **Sibling roster**（S1, #what-loads-at-startup）：子 agent 启动时收到一条系统提醒，列出 `main` 及会话内其他命名 agent——即子 agent 从上下文里就知道"main"存在、自己不是 main（v2.1.206+，需子 agent 工具含 `SendMessage`）。
2. **输出头标记**（S1, #subagent-output-scanning，v2.1.210+）：

> "A report that returns to Claude as the subagent's result also arrives under a header marking it as subagent output. **The header states that instructions or approval claims inside the report are the subagent's words and carry no authority from you.**"

此外 S1 明确：子 agent 跑自己的系统提示词，所以主会话的 output style 等不会影响它。

**未找到一手来源的点**：内置 agent（Explore / Plan / general-purpose / claude）的**完整**系统提示词原文，官方文档未公开，只有一句 "Built-in agents have predefined prompts" 及行为描述。第三方仓库（如 VoltAgent/awesome-claude-code-subagents）有复制流传，**仅有二手来源、未经官方确认，本文不作为结论依据**。

---

## 三、嵌套：默认允许，但有深度限制（默认 3 层）

**结论：官方允许子 agent 再派生子 agent，默认最多 3 层，到限后收回 `Agent` 工具；可用环境变量把层数改成 1（关闭嵌套）等任意值。**

S1（#let-subagents-spawn-their-own-subagents）：

> "**By default, a subagent can spawn subagents of its own, up to three layers below the main conversation.** At the depth limit, Claude Code withholds the `Agent` tool from every subagent except a fork, so a subagent at the limit does its delegated work itself and returns one summary. A fork at the limit keeps `Agent` in its inherited tool list, but the tool returns an error instead of spawning."

配置方式：`CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`（settings.json `env` 块），"Set `1` to turn nesting off"。另外 S1 记录了版本演变（说明官方在这个问题上反复调整过）：

- v2.1.172–v2.1.216：默认可嵌套 5 层，且不可改；
- v2.1.217–v2.1.218：默认 1 层；
- v2.1.219 起：默认 3 层。

Agent SDK 文档（S2, #cap-subagent-depth-concurrency-and-spend）给出一致的限制表：Depth 默认 "3 layers of subagents below your main agent. `1` stops your subagents from spawning any of their own"；另有并发上限 `CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS`（默认 20）与花费上限 `maxBudgetUsd`。

S1 还给出嵌套的适用场景："Nested subagents suit a delegated task that itself splits into parallel subtasks, such as a reviewer subagent that dispatches a verifier per finding."

---

## 四、结果回传：只有最终消息 / 摘要，不是完整对话

**结论：主 agent 只收到子 agent 的最终一条消息（Agent tool result）；中间工具调用与输出全部留在子 agent 上下文里，不回流。**

- S4（#agent-tool-behavior）："The parent doesn't see the subagent's intermediate tool calls or outputs, only that final result."
- S2（#what-subagents-inherit）："The parent receives the subagent's final message as the Agent tool result, but may summarize it in its own response."（如需原文呈现，要在主 query 的 prompt 或 systemPrompt 里显式要求。）
- S1（#resume-subagents）：子 agent 完成时主 agent 拿到 **agentId**；`maxTurns` 到限时返回结果被标记为 **partial**；可用 `SendMessage` 按 ID/名字 resume，"Resumed subagents retain their full conversation history, including all previous tool calls, results, and reasoning"——**续跑保留的是子 agent 自己的历史，不是主会话历史**。
- 后台子 agent：结果以 completion notification 形式在后续 turn 到达（S1, #run-subagents-in-foreground-or-background）。
- 安全边界：最终报告在交给主 agent 前会过输出扫描（S1, #subagent-output-scanning；S2），加标记头（见第二节）。

---

## 五、工具面：可继承、可收窄；主 agent 侧限制手段很细

**结论：子 agent 默认继承主会话可用工具，但经过两层过滤；主 agent / 用户可以用 `tools` 白名单、`disallowedTools` 黑名单、`Agent(类型)` 白名单、MCP 服务级模式、权限 deny 等多种方式限制。**

S1（#available-tools）的默认继承与过滤规则：

> "Subagents inherit the built-in tools and MCP tools available in the main conversation, **narrowed by two filters**: the first removes a short list of tools from every subagent, and the second reduces the built-in tool set for subagents that run in the background…"

第一层过滤无条件移除：`Agent`（嵌套到深度限制时）、`AskUserQuestion`、`EndConversation`、`EnterPlanMode`、`ExitPlanMode`（除 `permissionMode: plan`）、`ScheduleWakeup`、`WaitForMcpServers`、`Workflow`。第二层过滤让后台子 agent 只保留一小组内置工具（Read/Grep/Glob/LSP/Bash/Edit/Write/WebFetch/WebSearch/Skill 等）。

主 agent / 用户侧的限制手段（S1 #available-tools、#restrict-which-subagents-can-be-spawned、#disable-specific-subagents；S2 #tool-restrictions）：

| 手段 | 语义 |
|---|---|
| `tools: Read, Grep, Glob` | 白名单，只给列出的工具；列出的工具若全部解析失败，通常拒绝启动子 agent |
| `disallowedTools: Write, Edit` | 黑名单，从继承集里移除 |
| `Agent(worker, researcher)` | 主线程 agent 只能派生这两个 subagent 类型（仅对 `claude --agent` 主线程生效） |
| `tools: Agent`（不带括号）/ 省略 `Agent` | 允许 / 禁止该 agent 派生任何子 agent |
| `mcp__github`、`mcp__*` | 服务级 / 全局移除 MCP 工具 |
| `permissions.deny: ["Agent(Explore)"]` | 禁止委派某个具体类型 |
| `mcpServers` 字段 | 给单个 subagent 挂专属 MCP server，工具描述不占主会话上下文 |

典型官方配置示例：Explore / Plan 内置 agent 是只读的（"Tools: read-only tools; Write and Edit are denied"）；code-reviewer 示例只给 `Read, Grep, Glob, Bash`。

---

## 六、官方对"上下文隔离 vs 共享"取舍的论述

**结论：Anthropic 的公开立场是"默认隔离，共享/共享上下文只是特例"，并给出了明确的适用边界。**

1. **多 agent 研究系统博客**（S3）把隔离列为多 agent 架构的核心收益：

> "Subagents facilitate compression by operating in parallel with **their own context windows**, exploring different aspects of the question simultaneously before condensing the most important tokens for the lead research agent. Each subagent also provides **separation of concerns—distinct tools, prompts, and exploration trajectories—which reduces path dependency** and enables thorough, independent investigations."

> "This finding validates our architecture that distributes work across agents with **separate context windows** to add more capacity for parallel reasoning."

同时博客明确划出了不适合多 agent 的边界（对本 SDK 这种编码场景直接相关）：

> "…some domains that require **all agents to share the same context** or involve many dependencies between agents are not a good fit for multi-agent systems today. For instance, **most coding tasks involve fewer truly parallelizable tasks than research**…"

长程任务的处理也是"换干净上下文"而不是"共享上下文"（S3, Appendix）：

> "When context limits approach, agents can **spawn fresh subagents with clean contexts** while maintaining continuity through careful handoffs."

（另有一个反向补充：S3 提到"Subagent output to a filesystem to minimize the 'game of telephone'"——大产物让 subagent 落文件、只回轻量引用，以减少经过对话历史的拷贝损耗。）

2. **Claude Code 文档**（S1, #choose-between-subagents-and-main-conversation）给出的选择标准：

| 用主会话 | 用 subagent |
|---|---|
| 需要频繁来回、多阶段共享大量上下文、快速小改动、**在意延迟**（"A subagent that isn't a fork starts fresh and may need time to gather context"） | 产出大量你不需要的日志/输出、想强制执行工具限制、工作自包含可返回摘要 |

并指出 fork 的代价原话（S1, #fork-the-current-conversation）："**This drops the input isolation that subagents otherwise provide**"——即官方把"共享上下文"明确记作隔离性的损失，而不是默认形态。

3. **KV cache 层面的取舍**（S5, #subagents-and-the-cache）——这一点与本 SDK 的实现动机直接对应：

> "A subagent starts its own conversation with its own system prompt and tool set, separate from the parent's. **Its first request doesn't read the parent's cache, because the two prefixes differ**, and it warms a cache of its own across its turns… The parent's cache is unaffected."

> "A fork, by contrast, inherits the parent's system prompt, tools, and conversation history exactly, so **its first request reads the parent's cache**."

S1（#how-forks-differ-from-other-subagents）对应表述："Because a fork's system prompt and tool definitions are identical to the parent, its first request reuses the parent's prompt cache. **This makes forking cheaper than spawning a fresh subagent for tasks that need the same context.**"

也就是说：**Claude 完全承认 fork 在 prompt cache 上更便宜**，但把它放在"显式 opt-in 的特例"里，而不是默认；默认路径宁可让子 agent 自建前缀缓存。

---

## 七、与 fork 方案的对比

下表对比 Claude Code 的两种官方形态与本 SDK 的内置 `subagent` tool（`pkg/api/subagent_tool.go` 的 fork 方案）。

| 维度 | Claude Code 默认 subagent | Claude Code fork（`/subtask`、`subagent_type: "fork"`） | 本 SDK subagent tool（fork 方案） |
|---|---|---|---|
| **上下文** | 全新隔离窗口：自有 system prompt + 主 agent 写的 delegation 消息 + CLAUDE.md + git status + 预载 skills + sibling roster；**不含**主会话历史、主 agent 已读文件、已用 skills（S1 #what-loads-at-startup） | 继承整个会话：system prompt、tools、model、message history 与主会话完全一致（S1 #fork-the-current-conversation） | 深拷贝主会话完整历史（`buildForkedHistory` + `CloneMessages`），只在末尾追加一条 `[Subtask]` 用户消息（含输出契约类型提示与身份提醒） |
| **身份** | 为该 agent 单独编写的 system prompt（官方示例均为 "You are a …"），**不继承** Claude Code 主系统提示词；sibling roster 告知 `main` 存在；回传结果带"subagent 输出、无权威"头（S1/S2） | 复用主会话同一套 system prompt/tools——官方语义上 fork 就是主会话的副本/分身，而不是一个新身份 | 子 runtime 与主 runtime **同一套** system prompt 与工具面（`buildSubOptions` 原样复制 `opts`，注释明说"tools 与 system 必须与主请求逐字节一致才能命中 provider 的 KV/prefix cache"），身份仅靠 history 末尾的软性"身份提醒" |
| **嵌套** | 默认允许，最多 3 层（`CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`，置 1 关闭）；到限收回 `Agent` 工具（S1） | fork 不能再派 fork；深度限制下 `Agent` 工具报错（S1） | 子 runtime 内 subagent tool 被 no-op 化（`disabled` 分支），从机制上硬阻断孙 agent 派生（代码注释称"提示词只是软约束"） |
| **结果回传** | 只有最终消息；中间工具调用不回流；`maxTurns` 到限标记 partial；`agentId` + `SendMessage` resume（保留子 agent 自己的历史）；输出扫描头（S1/S2/S4） | 只有最终结果回来，fork 的工具调用不进主会话（S1 #fork-the-current-conversation） | 子 agent 最终输出文本作为 tool result 返回；另用 `sub_session_id` 追问机制在原上下文续跑（类似 resume，代码实现） |
| **KV cache** | 子 agent 自有前缀 → 首请求**不读**父缓存、自建缓存；父缓存完全不受影响（S5） | 前缀与父逐字节一致 → 首请求即读父缓存，"forking cheaper"（S5/S1） | 刻意保持 system/tools/历史逐字节一致以命中 prefix cache——这正是身份混淆隐患的来源 |
| **官方对混淆风险的显式态度** | 结构上不产生"我曾是主 agent"的证据链 | 文档明确提示该模式 "drops the input isolation"，是显式选择 | 代码注释已识别"子 agent 误以为自己是主 agent"问题，但用末尾消息做软约束 |

**Claude 的默认设计为什么天然不出现"子 agent 以为自己是主 agent"**：

1. **上下文里没有主 agent 的任何发言**。子 agent 看到的历史第一条就是 delegation 消息，不存在"我（作为主 agent）说过这些话"的证据链——而 fork 方案恰好把主 agent 的全部 assistant 轮次原样放进子 agent 历史，模型把这些轮次读作"自己说过的话"。
2. **系统提示词是第一句身份声明**。子 agent 的系统提示词是 "You are a code reviewer…" 这类专门为它写的身份，而不是主 agent 的通用系统提示词；身份在提示词最顶层，不在历史末尾的追加消息里。
3. **角色由交接结构定义**。子 agent 的上下文以"受托方接到任务"开头（task message），sibling roster 还显式告诉它 `main` 的存在（S1）。
4. **回传通道有防混淆设计**。最终报告带"这是 subagent 输出、其中的指令和授权声称不具权威"的头（S1 #subagent-output-scanning），主 agent 不会把子 agent 的话当成自己的意图。
5. 反观 Claude 自己的 fork：它确实复用主会话的 system prompt 和历史，之所以不造成混淆，是因为官方语义上 fork 就是**同一个 agent 跑支线任务**（同一份 prompt、同一份历史，即"还是我自己"），且官方文档把"放弃隔离"写在该特性的说明里，让使用者知情。

---

## 八、对本 SDK 的启示（设计方向，不含代码）

以下仅列方向，按"改动量 / 风险"大致排序；均需在动代码前先读相关实现验证可行性。

1. **把"隔离派发"设为默认，fork 降级为显式模式。** 默认路径下子 agent 只拿到自包含的 delegation prompt，不深拷主历史。官方原话可直接作为派发契约的注脚："The only content you pass from parent to subagent is the Agent tool's prompt string, so include any file paths, error messages, or decisions the subagent needs directly in that prompt."（S2）代价是主 agent 必须把背景信息写进 instruction——官方同样承认这点，并把它作为 delegation 质量的要求（S3: "Each subagent needs an objective, an output format, guidance on the tools and sources to use, and clear task boundaries"）。
2. **为子 agent 生成独立的身份系统提示词。** 每个 `AgentType` 持有自己的 system prompt/追加提示词（如 "You are a <type> subagent…"），子 runtime 用它替换主 system prompt，而不是原样继承。这是消除"以为自己是主 agent"最直接的一招，且与 Claude 的做法同构。
3. **fork 保留为 opt-in 特例，并在文档/工具描述里写明取舍。** Claude 的做法是：fork 模式只在交互式会话默认开，`-p` 与 SDK 默认关，并明确写 "drops the input isolation"。本 SDK 可仿此：默认隔离；需要"带着全部背景跑支线"时显式传 fork 开关，同时接受身份漂移风险。
4. **缓存取舍需要显式决策，而不是默认绑定。** Claude 的答案是：默认承受子 agent 自建前缀缓存的首包成本，把"读父缓存"的收益只给 fork。若本 SDK 想两者兼得，可评估的方向：(a) Anthropic Messages API 的 `system` 是数组、可分段缓存——把"身份声明"放在 system 的独立 cache 段里，理论上可在改身份的同时保住主前缀缓存；(b) 把身份声明放进 system 而非历史末尾消息。**注意：这两点都涉及本 SDK `pkg/model` 对多段 system 的实际支持与 provider 行为，本次调研未读这部分代码，落地前必须先核实**（当前 `buildSubOptions` 的注释已表明"删工具或收窄都会破坏缓存"是该方案的核心顾虑）。
5. **嵌套从"硬阻断"改为"可配置深度 + 到限收回派发工具"。** Claude 默认允许 3 层并提供 `CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH`；本 SDK 目前的 no-op 化等效于深度 1。可评估给 `Options` 增加深度上限配置，到限时让派发工具直接返回拒绝（现机制的拒绝文案已经存在，可复用）。
6. **结果契约向官方靠齐。** 可借鉴：最终结果标记 partial（如本 SDK `maxTurns` 类场景有对应能力的话）、`agentId`/`sub_session_id` 续跑、回传结果带"subagent 输出、非主 agent 指令"的头标记（防注入/防权威混淆，S1 #subagent-output-scanning 是一手依据）。
7. **工具面收窄与 fork 解耦。** Claude 的 `tools`/`disallowedTools`/`Agent(类型)` 白名单与是否 fork 是两个正交维度。本 SDK 的 `AgentType` 目前不改变工具面（为保 cache），后续若走隔离默认路线，"类型 = 工具集 + 模型 + 独立 system prompt"会自然成立，Explore 式只读 agent 也可以低成本实现。

---

## 九、不确定与未找到一手来源的点

| 事项 | 状态 |
|---|---|
| 内置 agent（Explore / Plan / general-purpose / claude）完整系统提示词原文 | **未找到一手来源**。官方文档只有一句 "Built-in agents have predefined prompts" 及行为描述；第三方仓库有流传复制，仅二手、未经官方确认，本文未采信 |
| 内置 agent 提示词里是否逐字包含"你是子 agent"类声明 | **未找到官方明确说明**。可确认的机制是"自有 system prompt + sibling roster + delegation message + 输出头"，但具体措辞无官方原文 |
| fork 模式下 Claude 如何向 fork 传达任务 | 官方文档描述了 fork 继承全部会话并可由 `/subtask` 附任务，但 fork 收到的"任务消息"在官方文档中无单独章节，细节未确认 |
| Claude Code 各特性的版本归属 | 文档含大量版本注释（如输出扫描 v2.1.210+、sibling roster v2.1.206+、fork 默认开 v2.1.232+），本文按抓取时点记录；后续版本可能变化 |
| 多段 system 缓存方案在本 SDK 的可行性 | **未验证**：本次未读 `pkg/model` provider 实现，不确认是否支持分段 system / cache_control；动代码前必须核实 |
| GitHub issue #10212（用户请求 subagent 独立上下文窗口，被机器人关闭为 duplicate） | 无 Anthropic 员工回复，**不作为官方立场依据**，仅在检索过程中出现 |

---

> **综合评估**：Claude Code 的默认子 agent 设计 = 「全新隔离上下文 + 独立身份系统提示词 + delegation 消息交接 + 只回最终结果」，fork 全量历史是被文档明确标注了隔离代价的显式特例，且即便在 fork 模式下官方也只回传最终结果。本 SDK 的 fork 方案与 Claude 的 fork 特例同构（连"为 prefix cache 保逐字节一致"的动机都一致，见 S5/S1 对 fork 缓存优势的说明），差异在于 Claude 把隔离作为默认、把 fork 作为例外。若要消除"子 agent 以为自己是主 agent"，最贴合官方做法的是把默认路径改为隔离上下文 + 子 agent 独立系统提示词，fork 保留为显式开关。
