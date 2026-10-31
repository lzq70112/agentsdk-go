# Spec：Auto Memory 记忆系统（comate 式移植）

> **日期**：2026-10-31
> **状态**：已评审待实施（分期：一期 → 二期）
> **背景**：对照 comate（comate_agent_sdk 0.12.21）的 auto memory 机制做完整移植。comate 侧机制调研见 im-robot 仓库 `docs/research/2026-10-31-comate-memory.md`；SDK 落点调研结论内嵌于本文"挂载点"标注。
> **约定**：记忆文件协议与 comate 逐字对齐（提示词原文、常量、新鲜度文案）；与 comate 的偏离点在 §8 显式列出并给理由。

---

## 1. 目标与非目标

**目标**：SDK 内置跨会话持久记忆——模型可显式读写记忆文件；每回合按需召回相关记忆注入上下文（带新鲜度警示与预算）；回合结束后后台子代理自动抽取新记忆。

**非目标（明确不做）**：
- dreaming 整理（comate 本机实测从未触发，投入产出比低，留待后续评估）；
- MEMORY.md 索引维护（comate 全盘实测零使用，召回只认主题文件 frontmatter）；
- settings.json 层的 memory 配置键（一期只走 Options；宿主按需接线）；
- 跨机同步、多用户隔离。

## 2. 存储设计

- **记忆目录（auto 来源）**：默认 `~/.agents/memory/<bucket>/`，`<bucket>` = 规范化 ProjectRoot 绝对路径（整体转小写，`\` `/` `:` 替换为 `-`，保留 CJK）。`Options.Memory.Dir` 非空时整体覆盖（宿主给绝对路径就用绝对路径）。
- **extra 来源（只读）**：`Options.Memory.ExtraDirs []string`，宿主挂载的用户手写知识目录；只参与召回，永不附过期警示，写入永远拒绝。
- **文件协议**（对齐 comate `memory_store.py`）：
  - 每条记忆一个主题 `.md`，YAML frontmatter：`name` / `description`（召回键，空白折叠后非空才有效）/ `type`（user|feedback|project|reference，缺省 reference）/ 可选 `enabled`（只认显式 false/no/off/0 停用）；
  - 扫描：`**/*.md`，排除 `MEMORY.md`（大小写不敏感），按 mtime 倒序**最多 200 文件**；frontmatter 只读前 **30 行**；非 UTF-8 跳过；
  - 单条 surface 截断：先 **200 行**、再 **4096 字节**，截断后追加 `> This memory file was truncated. Read the complete file at: <posix path>`；
  - manifest 行格式：`- [{type}] {filename} ({ISO8601 秒级 UTC Z}): {description}`。
- **常量**（comate 同款，硬编码）：`MAX_SCAN_FILE_COUNT=200`、`MAX_FRONTMATTER_LINES=30`、`MAX_SURFACED_MEMORY_BYTES=4096`、`MAX_MEMORY_LINES=200`、`MAX_SELECTED_MEMORIES=5`、`MAX_SESSION_SURFACED_BYTES=60000`、新鲜度阈值 **2 天**。

## 3. 配置（Options）

```go
type MemoryOptions struct {
    Enabled   bool      // 默认 true（withDefaults）
    Dir       string    // 空 = ~/.agents/memory/<bucket>/
    ExtraDirs []string  // 只读召回源
    SelectorTier ModelTier // 二期 side_query 选择模型档位，默认 ModelTierLow（无池则主模型）
}
```

- 加在 `pkg/api/options.go`（`Options.Memory`），`withDefaults` 派生默认目录与开关，`frozen()` 深拷贝 ExtraDirs。
- 挂载点：`New()`（agent.go:96-224）装配；落盘状态文件（二期游标）沿用 `cron_store.go` 的 tmp+rename 原子写。

## 4. 常驻提示（memory_usage）

- 渲染 comate `prompts/agent/memory_usage.md` 全文（`{{MEMORY_DIR_PATH}}` = 记忆目录绝对路径），作为独立 section 注入 system prompt：`builder.AddSection("memory-usage", rendered, 45)`（新常量，与现 `SystemPromptSectionMemory`（40，AGENTS.md 专用）区分；挂载点 agent.go:120 附近）。
- **门控**：`Memory.Enabled == false` 时整段不注入、召回不跑、写守卫不生效（comate setup.py:32-33 同款）。
- 子 runtime 经 `buildSubOptions` 继承 Options → memory_usage 自然进入子 agent，需在抽取子代理的 Options 中显式关闭（§7 防递归）。

## 5. 写守卫（一期）

- **沙箱放行**：`buildSandboxManager`（sandbox_bridge.go:31-77）在 Memory.Enabled 时 `fs.Allow(<memory dir>)`（auto 来源目录；ExtraDirs 只读、不 allow）。工作区外路径因此可写，宿主零接线。
- **内部检查**：`runtimeHookAdapter.PreToolUse`（options.go:646-698）在 SafetyCheck 之后、用户 hook 之前插入：
  - 工具 ∈ {write, edit} 且目标路径解析后落在记忆目录内 → **secret 扫描**（复用/新写 secret 检测，检出密钥模式即 deny，错误信息不含密钥明文）；通过则放行（SDK 无审批闸，"免审批"即不额外拦截）；
  - 目标在 ExtraDirs 内 → 一律 deny（只读）；
  - 其他路径 → 行为完全不变。
- 记忆写入的事实记录（供二期防双写）：本回合内成功 write/edit 落记忆目录 → 置会话级标志（含回合边界清位逻辑见 §7）。

## 6. 召回注入（一期：确定性选择器；二期换 LLM 选择器）

**挂载点**：`runLoop` 用户消息落 history 之后（`runtime_internal.go:178` 附近），**本回合首次模型调用前**完成注入。实现为直接向 `prep.history` append 一条合成 user 消息（**不走 `Runtime.Inject`**——DEFER 队列在回合末才 drain，会晚一回合）。

**管线**（comate `memory_runtime.py` 同款）：
1. **门槛**：回合文本 CJK ≥4 字符、否则 >1 词；subagent 上下文跳过；Memory/extra 均无则跳过。
2. **预算**：扫描 history 中既有注入项的 metadata（`origin=relevant_memory`）累计 `memory_total_bytes`，≥60KB 则本链路停跑；同会话已注入路径集合去重。
3. **扫描**：auto 目录 + ExtraDirs 各取 mtime 前 200 的 frontmatter（extra 文件名加 `extra-{i}/` 前缀防撞）。
4. **选择**（`memorySelector` 接口，两个实现）：
   - 一期 `keywordSelector`：回合文本与 description 做分词重叠打分（CJK 2-gram + 英文小写词），取分 >0 的前 5 条，分数并列按 mtime 新者优先；
   - 二期 `llmSelector`（§7.2）替换，接口不变。
5. **读取**：选中文件逐个按 §2 协议截断读取。
6. **渲染注入**：

```
<system-reminder>
Relevant memory surfaced for this turn:

{每条：[新鲜度警示空行分隔] + "{label}: {posix_path}" + 空行 + 内容；条间空行}
</system-reminder>
```

- 新鲜度（**本期核心，comate memory_runtime.py:404-436 逐字移植**）：
  - 天龄 = UTC 下 `max(0, (now - mtime) / 86400)`；0=今天、1=昨天、2+=更旧；
  - auto 来源 age ≥2 时在内容前附：`This memory is {N} days old. Memories are point-in-time observations, not live state — claims about code behavior or file:line citations may be outdated. Verify against current code before asserting as fact.`；
  - 标签：extra → `User-authored knowledge (always applies)`；auto → `Memory (saved today)` / `Memory (saved yesterday)` / `Memory (saved {N} days ago)`。
- 消息 metadata 记账：`origin=relevant_memory`、`memory_paths`、`memory_total_bytes`（压缩豁免标记与合成消息标记沿用 SDK 现有机制，参照 reminder.go:198-229 的 synthetic 处理）。
- 预算扣减：逐条累加，放不下的整条放弃。

## 7. 二期：LLM 选择器 + 后台抽取

### 7.1 抽取子代理（回合结束触发）

- **触发**：`endActiveRun`（cancel.go:43-53）**成功收尾路径**（completed，非 cancel/error；scheduled/cron 回合跳过；回合内必须有真实用户消息）。
- **实现**：SDK 内部仿 `dispatch`（subagent_tool.go:300-335）自建子 Options（不走 subagent tool）：`HistoryLoader` = `rt.SessionHistory` 克隆快照（"克隆父上下文"）；`ToolWhitelist` = {read, write, edit, glob, grep}（**不含 bash**，见 §8-3）；`MaxIterations` = 5；System = extract.md 渲染（`{{NEW_MESSAGE_COUNT}}` = 本回合新增消息数、`{{MEMORY_TYPE_GUIDE}}` = type_guide 全文、`{{EXISTING_MEMORIES}}` = manifest）；`Memory.Enabled = false`（防递归）。
- **游标与并发**：记忆桶目录下 `.memory_state.json`（tmp+rename 原子写）存 `last_cursor_message_id`；抽取进行中又来新回合 → pending 标记，当前完成后自动跑一轮合并抽取；全局并发 1。
- **防双写**：游标以来主对话已有成功的记忆目录写入（§5 标志）→ 跳过本轮抽取（comate memory_extract.py:243-281 同款）。
- **工具校验**：抽取子代理的 write/edit 只允许落记忆目录内主题 `.md`（拒绝 MEMORY.md、拒绝删除），越权即 deny。
- **生命周期**：Runtime.Close 时停掉在途抽取（参照 `subagent.shutdownAsync` agent.go:423）；抽取失败只 warn 日志，绝不影响主回合。

### 7.2 LLM 选择器（side_query 等价）

- `model.Model.Complete` 单次调用（不进主历史；compactor 先例 runtime_internal.go:242）：system = `select_relevant.md` 全文，user = `Query:\n{query}\n\nAvailable memories:\n{manifest}{Recently used tools: ...}`（近期工具 = 最近 8 个去重工具名）。
- 模型：`ModelPool[Memory.SelectorTier]`，无池用主模型。
- 响应解析：宽松 JSON（容忍围栏/前后杂文本），`{"selected_memories": [...]}`，截前 5 个非空名，按 filename 回选（未命中忽略）。
- 失败处理：warn 日志 + 本回合不注入（与 comate 一致，不做降级重试）。
- 触发挂载点与一期相同（runLoop :178 附近），**异步发出**：不阻塞等待——首次模型调用前未完成则本回合放弃注入（comate consume_ready 语义）。

## 8. 与 comate 的偏离点（及理由）

1. **存储位置**：`~/.agent/sessions/<桶>/memory/` → `~/.agents/memory/<bucket>/`（跟随 SDK 既有 `~/.agents` 用户级约定；桶名规范化算法同 comate）。
2. **一期选择器**：comate 只有 LLM 选择；我们一期先上确定性 keyword 选择器（可测、零模型依赖），二期换 LLM 选择器，接口不变。
3. **抽取无 Bash**：comate 允许只读 Bash，SDK 的 ToolWhitelist 无法限定"只读命令"，为安全直接不给 bash（extract.md 相应删去 bash 措辞）；抽取任务本身不需要 shell。
4. **无 side_query 抽象**：直接用 `model.Model.Complete`，不引入独立 side_query 包。
5. **LLM 选择器的启用条件**（实现期调整）：仅当 `ModelPool[Memory.SelectorTier]` 存在时启用 llmSelector；无模型池时**回退一期 keyword 选择器**而非用主模型——避免每回合多一次主模型同步调用，且注入成败不应依赖竞态。
6. **抽取的启用条件**（实现期调整）：`withDefaults` 从 nil 派生的默认配置带 `autoDerived` 标记，**不启用后台抽取**（仅保留召回语义）——否则每个 completed 回合都会静默发起抽取子代理（额外模型调用 + 写盘副作用），破坏"宿主未配置即无行为变化"的库默认语义。宿主显式配置 Memory（`WithMemoryOptions`）即启用完整抽取链路。
7. **抽取游标**：SDK 消息无稳定 ID，游标用**历史消息条数**（压缩致历史变短时收敛 `min(cursor, histLen)`）；抽取失败游标照常推进、不重试（对齐 comate 失败不重试语义）。

## 9. 测试与验收

**单元测试**（TDD，先写失败测试）：
- `memory_store_test.go`：frontmatter 解析（description 空白折叠/缺省跳过、enabled 显式停用、type 缺省 reference）、200 文件 mtime 截止、manifest 格式（UTC Z 时间戳）、200 行 + 4096B 双重截断与追加提示、非 UTF-8 跳过。
- `memory_recall_test.go`（stubModel + t.TempDir() + HOME 隔离 main_test.go 同款）：新鲜度三态（0/1/2 天 → 标签与警示文案逐字断言）、extra 来源永不附警示、60KB 预算停跑、同文件去重、CJK/英文门槛、keywordSelector 打分与并列、Enabled=false 全链路无痕。
- `memory_safety_test.go`：记忆目录 write/edit 放行 + secret 命中 deny（错误不含明文）、ExtraDirs 写入 deny、非记忆路径行为不变。
- 二期 `memory_extract_test.go`：触发条件矩阵（completed/cancel/error/scheduled/无用户消息）、工具越权 deny、游标推进与 pending 合并、防双写跳过、Memory 递归关闭。

**验收标准**：
- 一期：`make check`（fmt/lint/test/naming/duplicate）全绿；端到端——临时目录预置 3 个记忆文件（含 1 个 3 天前 mtime），跑一次真实回合，断言首条模型请求中含 `<system-reminder>` 包裹、新鲜度警示、预算与去重行为。
- 二期：抽取子代理产物落记忆目录且不触碰目录外路径；side_query 选择器对 stubModel 的 JSON 响应正确回选、垃圾响应静默不注入。

## 10. 涉及文件清单

**新增**（均在 `pkg/api/`）：`memory_store.go`、`memory_options`（并入 options.go）、`memory_prompt.go`、`memory_recall.go`、`memory_safety.go`、`memory_extract.go`（二期）、`prompts` 内嵌（Go embed 或常量字符串）、各 `*_test.go`。
**修改**：`pkg/api/options.go`（MemoryOptions + withDefaults + frozen）、`pkg/api/agent.go`（New 装配 + Close 收尾）、`pkg/api/runtime_internal.go`（runLoop 召回挂载）、`pkg/api/prompt_builder.go`（section 常量）、`pkg/api/cancel.go`（endActiveRun 抽取触发，二期）、`pkg/api/sandbox_bridge.go`（fs.Allow）。

**已知风险**：
- `SessionStart` 钩子 SDK 内无触发点——所有"会话启动"逻辑放 `New()`/runLoop，不依赖该钩子；
- 合成消息的 metadata 标记机制需与 `compact_preserve.go` 的豁免判定对齐，实现时以现状代码为准核实字段名；
- 子 runtime 会完整重建（重载 skills/MCP），抽取子代理用自建轻量 Options 绕开。
