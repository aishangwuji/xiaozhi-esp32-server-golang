package controllers

// NPC 工具目录总览（工具描述服务端化·阶段一的可观测补丁）
//
// 解决的问题：npc_tool_manifest 只做覆盖/摘除，但运营无法回答三个问题：
//  1. 这个 NPC 到底能看到哪些工具？
//  2. 我写的清单条目有没有命中真实工具？（toolmanifest.Apply 对未知工具名静默跳过）
//  3. 还有哪些工具没被登记进目录？
//
// 数据来源：
//   - 声明式快照 services/toolcatalog：本地工具 + 固件设备侧工具（代码里固定存在的集合）
//   - 设备实时上报：device_id 存在且设备在线时，向设备要一次 tools/list
//   - 清单条目：npc_tool_manifest 的全局 + agent 专属条目（agent 优先，与主服务预合并语义一致）
//
// 只读接口，不修改任何状态。

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"xiaozhi/manager/backend/models"
	"xiaozhi/manager/backend/services/toolcatalog"

	"github.com/gin-gonic/gin"
)

type npcToolOverviewItem struct {
	Name             string   `json:"name"`
	Sources          []string `json:"sources"` // declared:local / declared:firmware / live:device
	Declared         bool     `json:"declared"`
	Live             bool     `json:"live"`
	InManifest       bool     `json:"in_manifest"`
	ManifestScope    string   `json:"manifest_scope,omitempty"` // global | agent
	Enabled          bool     `json:"enabled"`                  // 最终是否进入 LLM 工具目录
	DescOverridden   bool     `json:"desc_overridden"`
	SchemaOverridden bool     `json:"schema_overridden"`
	Conditional      bool     `json:"conditional"` // 固件条件编译项
}

type npcToolUnmatchedEntry struct {
	AgentID  string `json:"agent_id"`
	ToolName string `json:"tool_name"`
	Enabled  bool   `json:"enabled"`
	Remark   string `json:"remark"`
	Reason   string `json:"reason"`
}

// GetNpcToolManifestOverview GET /api/admin/npc-tools/overview?agent_id=&device_id=
//
// agent_id 为空时只应用全局清单；device_id 为空时不拉设备实时工具（只用声明式快照）。
func (ac *AdminController) GetNpcToolManifestOverview(c *gin.Context) {
	if ac == nil || ac.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabaseUnavailable.Error()})
		return
	}
	agentID := strings.TrimSpace(c.Query("agent_id"))
	deviceID := strings.TrimSpace(c.Query("device_id"))

	catalog, err := toolcatalog.Get()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "工具目录快照解析失败: " + err.Error()})
		return
	}

	// ---- 1. 生效清单：全局条目 + agent 专属条目，agent 优先 ----
	var rows []models.NpcToolManifest
	if err := ac.DB.Where("agent_id = ? OR agent_id = ?", agentID, "").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询工具清单失败"})
		return
	}
	effective := make(map[string]models.NpcToolManifest, len(rows))
	for _, row := range rows {
		if row.AgentID != agentID {
			if _, taken := effective[row.ToolName]; taken {
				continue // agent 专属已占位
			}
		}
		effective[row.ToolName] = row
	}

	// ---- 2. 声明式目录 ----
	items := make(map[string]*npcToolOverviewItem, len(catalog.Tools))
	ensure := func(name string) *npcToolOverviewItem {
		if it, ok := items[name]; ok {
			return it
		}
		it := &npcToolOverviewItem{Name: name, Enabled: true}
		items[name] = it
		return it
	}
	for _, t := range catalog.Tools {
		it := ensure(t.Name)
		it.Declared = true
		it.Conditional = t.Conditional
		it.Sources = appendUnique(it.Sources, "declared:"+t.Source)
	}

	// ---- 3. 设备实时工具（可选） ----
	deviceNote := ""
	if deviceID != "" {
		if ac.WebSocketController == nil {
			deviceNote = "WebSocket 控制器未就绪，跳过设备实时工具"
		} else {
			var device models.Device
			if err := ac.DB.Where("id = ?", deviceID).First(&device).Error; err != nil {
				deviceNote = "设备不存在：" + deviceID
			} else if tools, err := ac.WebSocketController.RequestDeviceMcpToolDetailsFromClient(context.Background(), device.DeviceName); err != nil {
				deviceNote = "设备离线或读取失败：" + err.Error()
			} else {
				for _, t := range tools {
					name := strings.TrimSpace(t.Name)
					if name == "" {
						continue
					}
					it := ensure(name)
					it.Live = true
					it.Sources = appendUnique(it.Sources, "live:device")
				}
				// 设备离线时底层返回的是"空列表且 err=nil"，不区分就会让运营误判成
				// "设备在线但没有工具"。这里显式说明，避免误读。
				if len(tools) == 0 {
					deviceNote = "设备未上报任何工具（可能离线，或尚未完成 tools/list）"
				} else {
					deviceNote = ""
				}
			}
		}
	}

	// ---- 4. 应用清单，并找出未命中条目 ----
	unmatched := make([]npcToolUnmatchedEntry, 0)
	for name, row := range effective {
		it, ok := items[name]
		if !ok {
			unmatched = append(unmatched, npcToolUnmatchedEntry{
				AgentID:  row.AgentID,
				ToolName: row.ToolName,
				Enabled:  row.Enabled,
				Remark:   row.Remark,
				Reason:   "工具不在声明式目录中，且设备未上报（名字打错或该工具已下线）",
			})
			continue
		}
		it.InManifest = true
		it.Enabled = row.Enabled
		it.DescOverridden = strings.TrimSpace(row.Description) != ""
		it.SchemaOverridden = strings.TrimSpace(row.InputSchema) != ""
		if row.AgentID == "" {
			it.ManifestScope = "global"
		} else {
			it.ManifestScope = "agent"
		}
	}

	// ---- 5. 排序输出 ----
	list := make([]npcToolOverviewItem, 0, len(items))
	for _, it := range items {
		list = append(list, *it)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Enabled != list[j].Enabled {
			return list[i].Enabled // 被摘除的排后面
		}
		if list[i].InManifest != list[j].InManifest {
			return !list[i].InManifest // 已登记的排前面
		}
		return list[i].Name < list[j].Name
	})
	sort.Slice(unmatched, func(i, j int) bool { return unmatched[i].ToolName < unmatched[j].ToolName })

	// ---- 6. 统计 ----
	disabled, unlisted, live, declared := 0, 0, 0, 0
	for _, it := range list {
		if !it.Enabled {
			disabled++
		}
		if !it.InManifest {
			unlisted++
		}
		if it.Live {
			live++
		}
		if it.Declared {
			declared++
		}
	}

	services, err := listEnabledGlobalMCPServiceNames(ac.DB)
	if err != nil {
		services = []string{}
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"agent_id":    agentID,
		"device_id":   deviceID,
		"device_note": deviceNote,
		"board":       catalog.Board,
		"totals": gin.H{
			"tools":       len(list),
			"declared":    declared,
			"live":        live,
			"in_manifest": len(effective) - len(unmatched),
			"entries":     len(effective),
			"unlisted":    unlisted,
			"disabled":    disabled,
			"unmatched":   len(unmatched),
		},
		"tools":               list,
		"unmatched_entries":   unmatched,
		"global_mcp_services": services,
	}})
}

func appendUnique(list []string, value string) []string {
	for _, v := range list {
		if v == value {
			return list
		}
	}
	return append(list, value)
}
