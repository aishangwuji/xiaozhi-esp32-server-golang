# NPC 工具：注册、分配与机制化

面向"一个服务端、多个 NPC、每个 NPC 一套机制"的落地说明。
含代码位置、扩展步骤、以及当前实现里的硬约束。

---

## 1. 心智模型：注册 ≠ 分配

工具分四处注册，**注册只决定"池子里有什么"，按 NPC 分配由两道闸决定**。

| 池子 | 注册位置 | 粒度 | 被闸一管 | 被闸二管 |
|---|---|---|---|---|
| 本地工具 | `internal/app/server/chat/local_mcp_*.go` | 进程全局 | ✗ | ✓ |
| 全局 MCP 服务 | `mcp.global.servers` / 控制台 MCP 配置 | 服务级 | ✓ | ✓ |
| 设备 `self.*` | 固件 `main/mcp_server.cc` 的 `AddTool` | 设备级 | ✗ | ✓ |
| agent WS 端点 | agent 主动连入上报 | agent 级 | ✗ | ✓ |

- **闸一**：`Agent.mcp_service_names`（服务级白名单，`internal/domain/mcp/mcp_client.go:34`）
- **闸二**：`npc_tool_manifest`（工具级覆盖/摘除，`internal/domain/mcp/toolmanifest/toolmanifest.go`）

聚合顺序在 `internal/domain/mcp/mcp_client.go:268`：本地 → 全局（过滤）→ 设备 → agent 端点 → 应用闸二。
同名冲突时**先到者赢**，后者静默丢弃（本地 > 全局 > 设备）。

---

## 2. 新增一个工具

### 2.1 服务端本地工具（全局生效，不能按 NPC 分）

```go
// internal/app/server/chat/local_mcp_xxx.go
type MyParams struct {
    Target string `json:"target" description:"目标名，如 north_gate" required:"true"`
}

func init() {
    RegisterLocalMcpFunc("my_tool", "什么时候该调它……", MyParams{}, myHandler)
}

func myHandler(ctx context.Context, argsJSON string) (string, error) { /* ... */ }
```

- 参数 schema 由 struct tag 反射生成（`local_manager.go:69`），不用手写 JSON Schema
- 要配置开关就写进 `InitChatLocalMCPTools` 的 map（`local_mcp.<工具名>`）；`init()` 注册的没有开关
- 优先用 `RegisterLocalMcpFunc`；`control_music_playback` 就是这么注册的

### 2.2 机制型 MCP 服务（推荐，可按 NPC 分配）

复制 `cmd/mcp-forge`：

```bash
go run ./cmd/mcp-forge -selfcheck          # 不监听，先验证工具逻辑
go run ./cmd/mcp-forge -addr :18081        # 同时暴露 /sse、/message、/mcp
```

服务内工具用**短名**（`quote`/`craft`），服务端聚合时拼成 `<服务名>_<工具名>`
（`global_manage.go:625`）。控制台把服务名填 `forge`，模型看到的就是 `forge_quote`。

然后在控制台 `MCP 配置` 新增服务：

| 字段 | 值 |
|---|---|
| name | `forge`（机制 ID，短、唯一、不含下划线） |
| type | `sse` 或 `streamablehttp` |
| url | `http://<host>:18081/sse` 或 `http://<host>:18081/mcp` |

保存即热更（`cmd/server/main.go` 比对 `mcp` 段 → `App.ReloadMCP()`），无需重启。

---

## 3. 给不同 NPC 不同的工具

**机制 = 一个全局 MCP 服务；NPC = 一个 agent；agent 勾选它拥有的机制。**

控制台路径：`智能体 → 编辑 → MCP服务`（多选，留空 = 全部已启用服务）。

| 需求 | 手段 |
|---|---|
| NPC A 有锻造/任务，B 没有 | 闸一：A 勾 `forge,quest`，B 不勾 |
| 所有 NPC 都禁用 `self.server.exec` | 闸二：全局条目 `enabled=false` |
| A、B 都能交易，但语气不同 | 闸二：A 专属条目只改 `npc_trade` 的 description |
| 某 NPC 不要联网搜索 | 闸二：agent 专属 `web_search` = false（本地工具只能这么关） |

### 命名硬约束（都来自代码，不是建议）

1. **服务名互不为前缀**。闸一用 `strings.HasPrefix(toolKey, serviceName+"_")`，
   `shop` 会连带放行 `shop_advanced_*`。
2. **服务名不含下划线**，同理扩大会误匹配面。
3. **工具名只允许 `[A-Za-z0-9_-]`**，超 64 字符会被截断加 hash（`internal/domain/mcp/tool_name.go`）。
   中文名、带点号的名字会被改写——填闸二时必须用**改写后**的名字。
   设备侧 `self.camera.take_photo` 走另一条路径，沿用固件原名。
4. **单 NPC 工具数建议 ≤ 15**。每个工具都进 LLM 的 tools 数组，多了准确率掉；
   机制多时应做"进入某机制"再换工具集，而不是一次全塞。

---

## 4. 目录种子：把 23 个已有工具一次性登记

```bash
# 1. 盘点（会剔除被注释掉的注册项，还原常量名）
python esp32-watch/tools/tool_inventory.py --md esp32-watch/docs/npc-tool-inventory.md

# 2. 生成种子 SQL（23 条登记，其中 5 个高危 enabled=0）
python esp32-watch/tools/gen_manifest_seed.py --selftest --out esp32-watch/docs/npc-tool-seed.sql

# 3. 只给某个 NPC 装
python esp32-watch/tools/gen_manifest_seed.py --agent-id 12 --out seed-agent12.sql
```

种子只做两件事：**登记**（让控制台能看全并逐个开关）和**摘除高危**。
`description` 留空 = 不覆盖，仍沿用工具原始描述；要改写描述在控制台单条编辑。

高危摘除项：`self.server.exec`、`self.http.request`、`self.net.scan_lan`、
`self.net.check_ports`、`self.net.ping`。

执行前确认控制台库的方言（SQLite 默认，MySQL 加 `--dialect mysql`）：

```bash
sqlite3 <控制台.db> < esp32-watch/docs/npc-tool-seed.sql
```

---

## 5. 目录总览接口（看命中率）

```
GET /api/admin/npc-tools/overview?agent_id=7&device_id=14
```

- `agent_id` 空 → 只应用全局清单；`device_id` 空 → 不拉设备实时工具
- 返回每个工具的 `sources`（declared:local / declared:firmware / live:device）、
  `in_manifest`、`enabled`、`manifest_scope`、是否覆盖了描述/schema
- `unmatched_entries` = 清单里写了但**没有任何工具匹配**的条目
  （主服务 `toolmanifest.Apply` 对未知名字是静默跳过的，这是唯一能看到的地方）
- `totals.unlisted` = 还没登记进目录的工具数，是"目录完整度"指标

数据来源：`manager/backend/services/toolcatalog/local_tools.json`（构建期快照）
+ 设备实时上报 + `npc_tool_manifest`。快照与源码的一致性：

```bash
python esp32-watch/tools/tool_inventory.py \
  --check-catalog xiaozhi-golang-src/manager/backend/services/toolcatalog/local_tools.json
```

> 为什么用快照：`manager/backend` 是独立 Go module（`module xiaozhi/manager/backend`），
> 无法 import 主服务的 `internal/`。所以本地工具清单以静态 JSON 打包，靠上面的命令防漂移。

---

## 6. 已修的坑

- `NpcToolManifest.Enabled` 带 `gorm:"default:true"`，bool 零值在 `Create` 时会被列默认值顶掉，
  **新增一条 `enabled=false` 会存成 true**（摘除失效）。已在
  `controllers/npc_tool_manifest.go` 创建后显式补一次 `Update("enabled", false)`，
  并有回归测试 `TestUpsertNpcToolManifestPersistsDisabled`。
- 目录页文案写"只对设备侧工具生效"，与实际不符：闸二作用在聚合全集，
  本地工具与全局 MCP 工具都能被覆盖/摘除。
