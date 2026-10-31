# Spec：插件系统（.claude-plugin 标准格式）

> **日期**：2026-10-31
> **状态**：已评审待实施
> **背景**：为 SDK 增加 `.claude-plugin` 标准格式的插件系统（skills / agents / MCP / commands 四类资源），宿主（im-robot，即 charger/boomer）经 Admin UI 管理安装/启停。格式对齐 Claude Code 生态（comate、zcode 均已兼容该格式，本机实据：zcode 直接安装 anthropics/skills 官方插件包）。
> **调研结论**：SDK 现有机制与插件格式天然对齐——skills 走 `.agents/skills/*/SKILL.md`（pkg/runtime/skills，buildSkillsRegistry 于 pkg/api/runtime_helpers.go:207），subagents 走 `.agents/agents/*.md`（pkg/runtime/subagents/loader.go:77，frontmatter 解析为私有需加导出 API），MCP 走 settings.json `mcp.servers`（pkg/config/settings_types.go:23）+ collectMCPServers（pkg/api/mcp_bridge.go:25），hooks 走 settings.json Hooks。SDK 无 commands 概念，需新建。

## 1. 目标与非目标

**目标**：
- SDK 加载 `.claude-plugin` 格式插件，四类资源并入运行时：skills 并入 skills Registry、agents 并入 subagent 注册、MCP 并入 server 收集、commands 提供查询 API 由宿主触发；
- 安装器一步到位：marketplace（git clone）、zip 下载（sha256 校验）、local 目录三种来源；
- Admin UI 插件管理页：列表/安装/启停/卸载/marketplace 管理，重启进程生效。

**非目标（明确不做）**：
- 插件 hooks.json（不执行；comate 也未实现，保留字段不解析）；
- 热加载（安装/启停后需重启进程——用户已定；SDK 已有 ReloadSkill 属单 skill 场景，不扩展到插件）；
- npm/pip 分发（comate 同样只是预留）；
- 跨机同步、插件签名（sha256 完整性校验已覆盖 zip 场景）。

## 2. 插件格式（.claude-plugin 标准）

```
<plugin>/
├── .claude-plugin/
│   └── plugin.json          # 必需
├── skills/<skill-name>/SKILL.md
├── agents/<agent-name>.md   # YAML frontmatter + 正文（同 .agents/agents 约定）
├── commands/<command-name>.md
└── .mcp.json                # 可选，扁平格式（无 mcpServers 包裹）
```

**plugin.json 字段**：
- `name`（必填，缺失报错）、`description`（必填）、`version`、`author`（可选）；
- `skills`：可选，相对路径数组。**一旦出现（含空数组）即只按声明路径加载，默认 `skills/` 目录不再扫描**（对齐 comate 语义）；
- `mcpServers`：存档字段，加载器**不读**（对齐 comate——真正生效的 MCP 配置在插件根 `.mcp.json`）。

**.mcp.json（扁平格式）**：`{"server-name": {"type": "stdio"|"http", "command": ..., "url": ..., "env": ..., "timeoutSeconds": ...}}`——结构对齐 settings.json `mcp.servers` 的 entry（config.MCPServerConfig），零包裹。校验：type 只接受 stdio/http，其余条目报错跳过。

**commands/*.md 格式**：YAML frontmatter（`description` 用于列表展示；`argument-hint` 可选）+ 模板正文。占位符：`$ARGUMENTS`（全部参数原文）、`$1`..`$9`（按空白分词的位置参数，越界替换为空串）。渲染函数 `Render(args string) string`。

## 3. 插件根与注册表布局

默认 `<pluginsRoot>` = `~/.agents/plugins/`（对齐 SDK 用户级约定；`Options.PluginsDir` 可覆盖）：

```
<pluginsRoot>/
├── installed_plugins.json   # 已装清单（tmp+rename 原子写）
├── known_marketplaces.json  # 已知 marketplace
├── marketplaces/<name>/     # marketplace git clone --depth 1 本体
└── cache/<marketplace>/<plugin>/<version>/   # 插件物化目录
```

**installed_plugins.json 条目**：`{id: "name@marketplace", name, marketplace, version, installPath, installedAt, updatedAt, scope: "user", source: {type: marketplace|zip|local, url?, sha256?, path?}, enabled: true}`。local 来源 installPath 指向原目录（不拷贝，便于开发调试）。

## 4. 安装器（pkg/plugin 内实现）

- **marketplace add**：`git clone --depth 1 <url> <pluginsRoot>/marketplaces/<name>`（name 取 marketplace.json 的 `name`，git 场景可由仓库名兜底）→ 解析 `<clone>/.claude-plugin/marketplace.json`（顶层 `name` 必需，`plugins` 数组：每项 name/source/path 可选）；
- **install（marketplace 来源）**：按条目把 `<clone>/<path>` 复制到 `cache/<mp>/<name>/<version>/` → 登记 registry；
- **install（zip）**：HTTP 下载 → sha256 与条目声明比对（声明了才校验）→ archive/zip 解压（**Zip Slip 防御**：每个 entry 的清理后路径必须仍在目标目录内，`..` 与绝对路径一律拒绝）→ 可选 `stripRoot` 剥一层目录 → cache → 登记；
- **install（local）**：校验目标目录含 `.claude-plugin/plugin.json` → 直接登记绝对路径；
- **enable/disable**：改 registry 条目 `enabled` 字段（重启后生效）；**uninstall**：仅摘记录不删 cache（对齐 comate，便于重装）；
- **registry 读写**：tmp+rename 原子写（沿用 cron_store/memory_state 模式）；读损坏按空处理 + warn。
- git 可执行文件缺失时：marketplace add/install 报明确错误（zip/local 不受影响）。

## 5. 加载器（pkg/plugin）

```go
type Manifest struct { Name, Description, Version string; Author string; Skills []string /* nil=未声明 */ }
type Command struct { Name, Description, ArgumentHint, Template string }
type Plugin struct {
    Manifest   Manifest
    Dir        string
    Skills     []skills.SkillRegistration
    Agents     []subagents.SubagentRegistration
    MCPServers map[string]config.MCPServerConfig
    Commands   []Command
}
func Load(dir string) (Plugin, error)          // 单插件加载
func Discover(root string) ([]Plugin, error)   // 读 registry，enabled 插件逐个 Load；单个失败 warn 跳过
```

- skills：逐目录复用 **`skills.LoadSingleSkill`**（pkg/runtime/skills/loader.go:220 已导出）；
- agents：**给 pkg/runtime/subagents 新增导出 API** `LoadSingleSubagent(path string, fsLayer) (SubagentRegistration, error)`（内部复用 parseSubagentFile）；
- MCP：扁平 .mcp.json → map[string]config.MCPServerConfig（复用 config 类型与校验规则）；
- **冲突语义**：插件之间同 `name`（namespace）冲突 → 后加载整体丢弃 + warn（对齐 comate）；插件资源与宿主显式定义（Options.Skills/Subagents）重名 → **宿主优先**（对齐 comate merge 规则）；
- 无 namespace 前缀（SDK skill 名校验 isValidSkillName 不容冒号；重名靠上述丢弃/覆盖语义）。

## 6. Options 接线（pkg/api）

```go
// Options 新增
PluginsDir     string          // 非空则 New() 时 plugin.Discover 自动加载
Plugins        []plugin.Plugin // 宿主手动注入（与 PluginsDir 发现结果合并，插件名去重宿主优先）
DisabledPlugins []string       // 按插件名禁用（整个插件不加载，非单资源）
```

**New() 装配**（agent.go）：
- skills：插件 Skills 并入 `buildSkillsRegistry` 的 `fsRegs`（mergeSkillRegistrations 之前）；
- agents：插件 Agents 并入 subagent 注册来源（与 Options.Subagents 合并）；
- MCP：插件 MCPServers 并入 `collectMCPServers`（settings 之外的第三来源，命名冲突时 settings 优先；仍受 DisabledMCP 与 allow/deny 规则约束）；
- commands：存 runtime 字段，暴露 `Runtime.Commands() []plugin.Command` 与 `Runtime.RenderCommand(name, args string) (string, error)`——**SDK 不做触发**，宿主决定何时把指令渲染成 prompt。

## 7. 宿主接线（im-robot）

- **main.go 装配**：`Options.PluginsDir` 置默认 `~/.agents/plugins/`（可用 cfg.Agent.PluginsDir 覆盖）；
- **/指令触发**：钉钉/Admin UI 消息以 `/` 开头 → 按首个空白分词取指令名 → `Runtime.RenderCommand` 命中则渲染为 prompt 发起回合（未命中回落普通对话，保留现有 /命令如 /help 的宿主内置优先）；$ARGUMENTS = 指令名后的全部原文；
- **Admin UI**（im/admin + static）：
  - 后端 API（新 im/admin/plugins.go）：`GET /api/plugins`（已装列表 + 各 marketplace 可装清单）、`POST /api/plugins/install|enable|disable|uninstall`、`POST /api/plugins/marketplace/add|remove`——直接调 pkg/plugin 安装器；所有变更响应带 `needs_restart: true` 前端横幅提示；
  - 前端（app.js/index.html/style.css）：侧栏新增"插件"分组 tab：已装插件卡片（名称/版本/来源/启停开关/卸载）、marketplace 管理（添加 URL/浏览可装列表/一键安装）。**注意 Alpine `:disabled` 布尔归一化陷阱**（`!!(...)` 恒布尔，防 undefined 写 truthy 属性）；认证复用 admin Basic Auth。

## 8. 安全要点

- zip 解压 Zip Slip 防御（§4）；sha256 按声明校验并记入 registry 条目；
- 插件 skills/agents 不提权：走 SDK 既有工具白名单/沙箱；插件 MCP server 与 settings 配置的 server 同等对待（受 allow/deny 策略约束）；
- installPath 限定在 pluginsRoot 内（local 来源除外，显式登记绝对路径）；
- registry 写原子化，防半写损坏。

## 9. 与 comate/zcode 的偏离点

1. **hooks.json 不解析**（comate 读而不执行；我们连解析都不做，彻底不支持）；
2. **marketplace 物化用文件复制**（不做 git 细粒度 checkout/worktree）；
3. **commands 是 SDK 新增概念**：Claude Code/comate 的 commands 是 slash 命令；SDK 层只提供定义与渲染，触发语义归宿主（IM 机器人 /指令）；
4. **无 /plugin 交互式 picker**：安装走 Admin UI API（charger 是服务端，无 TUI）；
5. **单一 pluginsRoot**（不做 user/local 两级 scope；local 来源插件等效开发模式）。

## 10. 测试计划

**pkg/plugin**（表驱动）：plugin.json 解析（必填缺失/skills 字段覆盖默认目录语义/空数组陷阱）、四类资源发现（默认目录 vs manifest 声明）、.mcp.json 扁平解析与非法 type 拒绝、Command 渲染（$ARGUMENTS/$1-$9/越界空串）、zip 解压（stripRoot/Zip Slip 拒绝/sha256 校验）、marketplace.json 解析（name 兜底）、registry 原子写与损坏容错、Discover 冲突丢弃语义；安装器 git clone 用 `file://` 本地仓库实测，zip 用 httptest。
**pkg/api**：PluginsDir 自动发现、Plugins 手动注入合并、DisabledPlugins 整插件禁用、skills/agents/MCP 三路 merge、宿主显式定义优先、Runtime.Commands/RenderCommand。
**im-robot**：/指令端到端（stubModel：/cmd 消息 → 渲染 prompt 发起回合；未匹配回落）、admin plugins API（httptest：列表/安装/启停/卸载）。

## 11. 文件清单与分期

**SDK 仓库（feature/plugin-system 分支）**：
- 新增 `pkg/plugin/`：`manifest.go`（解析）、`loader.go`（Load/Discover）、`command.go`（渲染）、`installer.go`（marketplace/zip/local + registry）、`mcp.go`（扁平解析）、测试；
- 修改：`pkg/runtime/subagents/loader.go`（+LoadSingleSubagent 导出）、`pkg/api/options.go`（三字段 + frozen）、`pkg/api/agent.go`（装配 merge）、`pkg/api/mcp_bridge.go`（collectMCPServers 第三来源）、`pkg/api/runtime_commands.go`（Commands/RenderCommand）；
- spec 本文档。

**im-robot 仓库（main 分支）**：
- 修改：`cmd/im-robot/main.go`（PluginsDir 装配）、`im/robot.go`（/指令拦截）、`im/admin/`（plugins API + 路由）、`im/admin/static/`（插件 tab）；
- 新增：`im/admin/plugins.go`、相关测试。

**已知风险**：
- subagent frontmatter 字段与插件 agents/*.md 的一致性以 subagents 包现有 Metadata 为准；
- 插件 skill 与现有 `.agents/skills` 重名时的优先级已在 §5 定义（宿主目录优先于插件）；
- git clone 在无 git 环境的降级（§4 明确报错路径）。
