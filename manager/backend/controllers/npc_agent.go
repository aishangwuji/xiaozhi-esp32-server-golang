package controllers

// npc_agent.go — NPC 常驻 Agent 的控制台配置与运行状态代理
//
// 数据流（与 internal/domain/npcagent 对齐）：
//   控制台写入 npc_agent_config / npc_agent_settings
//     → 主服务每 60s 经 GET /api/internal/npc-agents 拉取并热更新调度计划
//     → 主服务 GET /internal/npc-agents/status 暴露运行状态（只读）
//     → 本控制器 GET /api/admin/npc-agents/status 代理该状态给前端
//
// 为什么状态要代理而不是前端直连主服务：主服务（8989）只对设备/工具侧开放，
// 且它的状态接口用内部服务 token 保护；控制台前端只认控制台的 JWT。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	npcAgentMaxAgentIDLen      = 64
	npcAgentMaxNameLen         = 64
	npcAgentMaxPersonaLen      = 2000
	npcAgentMaxRemarkLen       = 255
	npcAgentMinIntervalSeconds = 5
	npcAgentMaxIntervalSeconds = 3600
	// npcAgentSettingsRowID 全局设置固定单行
	npcAgentSettingsRowID = 1
	// npcAgentStatusTimeout 代理主服务状态接口的超时：主服务只是读内存快照，正常是毫秒级
	npcAgentStatusTimeout = 3 * time.Second
)

// npcAgentKnownActions 动作白名单。
// 三处必须严格一致：客户端 npc_act 的 enum、主服务 npcagent.DefaultAllowedActions、本文件。
var npcAgentKnownActions = []string{"walk", "run", "jump", "talk", "attack", "wait", "stop"}

// NpcAgentController NPC Agent 配置与状态
type NpcAgentController struct {
	DB                *gorm.DB
	InternalAuthToken string
	MainServiceURL    string
}

// NewNpcAgentController 构造控制器；主服务地址取 MAIN_SERVICE_URL，默认本机 8989（与内嵌部署一致）
func NewNpcAgentController(db *gorm.DB, internalAuthToken string) *NpcAgentController {
	baseURL := strings.TrimSpace(os.Getenv("MAIN_SERVICE_URL"))
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8989"
	}
	return &NpcAgentController{
		DB:                db,
		InternalAuthToken: internalAuthToken,
		MainServiceURL:    strings.TrimRight(baseURL, "/"),
	}
}

// npcAgentRequest 管理端写入请求体
type npcAgentRequest struct {
	ID              uint     `json:"id"`
	AgentID         string   `json:"agent_id"`
	Name            string   `json:"name"`
	Persona         string   `json:"persona"`
	IntervalSeconds int      `json:"interval_seconds"`
	Enabled         *bool    `json:"enabled"` // 指针区分"未传"与"传 false"
	AllowedActions  []string `json:"allowed_actions"`
	Remark          string   `json:"remark"`
}

type npcAgentSettingsRequest struct {
	Enabled                *bool `json:"enabled"`
	TickSeconds            int   `json:"tick_seconds"`
	DefaultIntervalSeconds int   `json:"default_interval_seconds"`
	DecideTimeoutSeconds   int   `json:"decide_timeout_seconds"`
	MaxStatusChars         int   `json:"max_status_chars"`
}

// validateNpcAgentInput 边界校验；返回规范化后的行与错误文案（空串=通过）
func validateNpcAgentInput(req *npcAgentRequest) (models.NpcAgentConfig, string) {
	row := models.NpcAgentConfig{
		ID:      req.ID,
		AgentID: strings.TrimSpace(req.AgentID),
		Name:    strings.TrimSpace(req.Name),
		Persona: strings.TrimSpace(req.Persona),
		Remark:  strings.TrimSpace(req.Remark),
	}

	if row.AgentID == "" {
		return row, "agent_id 不能为空（必须与游戏侧 MCP 通道 JWT 里的 agentId 一致）"
	}
	if len(row.AgentID) > npcAgentMaxAgentIDLen {
		return row, fmt.Sprintf("agent_id 过长（上限 %d 字符）", npcAgentMaxAgentIDLen)
	}
	if len(row.Name) > npcAgentMaxNameLen {
		return row, fmt.Sprintf("name 过长（上限 %d 字符）", npcAgentMaxNameLen)
	}
	if len(row.Persona) > npcAgentMaxPersonaLen {
		return row, fmt.Sprintf("persona 过长（上限 %d 字符）", npcAgentMaxPersonaLen)
	}
	if len(row.Remark) > npcAgentMaxRemarkLen {
		return row, fmt.Sprintf("remark 过长（上限 %d 字符）", npcAgentMaxRemarkLen)
	}

	interval := req.IntervalSeconds
	if interval == 0 {
		interval = 30
	}
	if interval < npcAgentMinIntervalSeconds || interval > npcAgentMaxIntervalSeconds {
		return row, fmt.Sprintf("interval_seconds 必须在 %d~%d 之间", npcAgentMinIntervalSeconds, npcAgentMaxIntervalSeconds)
	}
	row.IntervalSeconds = interval

	row.Enabled = true
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}

	actions, errText := normalizeNpcAgentActions(req.AllowedActions)
	if errText != "" {
		return row, errText
	}
	row.AllowedActions = actions
	return row, ""
}

// normalizeNpcAgentActions 去重、校验并序列化为逗号分隔；空输入表示使用默认全集
func normalizeNpcAgentActions(actions []string) (string, string) {
	if len(actions) == 0 {
		return "", ""
	}
	seen := make(map[string]struct{}, len(actions))
	out := make([]string, 0, len(actions))
	for _, raw := range actions {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if !containsString(npcAgentKnownActions, name) {
			return "", fmt.Sprintf("未知动作 %q（可用：%s）", name, strings.Join(npcAgentKnownActions, " / "))
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return "", ""
	}
	return strings.Join(out, ","), ""
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

// splitNpcAgentActions 把库里的逗号分隔值还原成数组（空=默认全集）
func splitNpcAgentActions(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ── 管理端：配置 CRUD ──

// GetNpcAgents GET /api/admin/npc-agents
// 返回配置行 + 主服务运行状态（状态拿不到不算错误，前端据此提示"主服务不可达"）
func (ac *NpcAgentController) GetNpcAgents(c *gin.Context) {
	var rows []models.NpcAgentConfig
	if err := ac.DB.Order("agent_id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取 NPC Agent 配置失败: " + err.Error()})
		return
	}

	settings, managed := ac.loadSettings()
	agents := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		agents = append(agents, gin.H{
			"id":               row.ID,
			"agent_id":         row.AgentID,
			"name":             row.Name,
			"persona":          row.Persona,
			"interval_seconds": row.IntervalSeconds,
			"enabled":          row.Enabled,
			"allowed_actions":  splitNpcAgentActions(row.AllowedActions),
			"remark":           row.Remark,
			"updated_at":       row.UpdatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"agents":   agents,
		"settings": settings,
		// settings_managed=false 表示库里还没有保存过全局设置，此时主服务仍按 main_config.yaml 跑
		"settings_managed": managed,
		"agents_managed":   len(rows) > 0,
		"runtime":          ac.fetchRuntimeStatus(),
		"known_actions":    npcAgentKnownActions,
	})
}

// UpsertNpcAgent POST /api/admin/npc-agents
func (ac *NpcAgentController) UpsertNpcAgent(c *gin.Context) {
	var req npcAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误: " + err.Error()})
		return
	}
	row, errText := validateNpcAgentInput(&req)
	if errText != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errText})
		return
	}

	// agent_id 是业务主键：同一个 agent 只能有一行，按 id 或 agent_id 命中都走更新
	var existing models.NpcAgentConfig
	query := ac.DB.Where("agent_id = ?", row.AgentID)
	if row.ID > 0 {
		query = ac.DB.Where("id = ?", row.ID)
	}
	err := query.First(&existing).Error
	switch {
	case err == nil:
		row.ID = existing.ID
		if err := ac.DB.Model(&models.NpcAgentConfig{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"agent_id":         row.AgentID,
			"name":             row.Name,
			"persona":          row.Persona,
			"interval_seconds": row.IntervalSeconds,
			"enabled":          row.Enabled,
			"allowed_actions":  row.AllowedActions,
			"remark":           row.Remark,
		}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败: " + err.Error()})
			return
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := ac.DB.Create(&row).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败: " + err.Error()})
			return
		}
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询失败: " + err.Error()})
		return
	}

	// 改完立即回读，避免前端把本地拼的对象当权威
	var saved models.NpcAgentConfig
	if err := ac.DB.Where("agent_id = ?", row.AgentID).First(&saved).Error; err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true,
		"agent": gin.H{
			"id":               saved.ID,
			"agent_id":         saved.AgentID,
			"name":             saved.Name,
			"persona":          saved.Persona,
			"interval_seconds": saved.IntervalSeconds,
			"enabled":          saved.Enabled,
			"allowed_actions":  splitNpcAgentActions(saved.AllowedActions),
			"remark":           saved.Remark,
			"updated_at":       saved.UpdatedAt,
		},
		"hint": "主服务会在 60s 内拉取到新配置并热更新，无需重启",
	})
}

// DeleteNpcAgent DELETE /api/admin/npc-agents/:id
func (ac *NpcAgentController) DeleteNpcAgent(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 id"})
		return
	}
	result := ac.DB.Delete(&models.NpcAgentConfig{}, id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败: " + result.Error.Error()})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "条目不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── 管理端：全局设置 ──

// GetNpcAgentSettings GET /api/admin/npc-agents/settings
func (ac *NpcAgentController) GetNpcAgentSettings(c *gin.Context) {
	settings, managed := ac.loadSettings()
	c.JSON(http.StatusOK, gin.H{"settings": settings, "managed": managed})
}

// UpdateNpcAgentSettings PUT /api/admin/npc-agents/settings
func (ac *NpcAgentController) UpdateNpcAgentSettings(c *gin.Context) {
	var req npcAgentSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误: " + err.Error()})
		return
	}
	settings, _ := ac.loadSettings()

	if req.Enabled != nil {
		settings.Enabled = *req.Enabled
	}
	if req.TickSeconds != 0 {
		if req.TickSeconds < 1 || req.TickSeconds > 60 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tick_seconds 必须在 1~60 之间"})
			return
		}
		settings.TickSeconds = req.TickSeconds
	}
	if req.DefaultIntervalSeconds != 0 {
		if req.DefaultIntervalSeconds < npcAgentMinIntervalSeconds || req.DefaultIntervalSeconds > npcAgentMaxIntervalSeconds {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("default_interval_seconds 必须在 %d~%d 之间", npcAgentMinIntervalSeconds, npcAgentMaxIntervalSeconds)})
			return
		}
		settings.DefaultIntervalSeconds = req.DefaultIntervalSeconds
	}
	if req.DecideTimeoutSeconds != 0 {
		if req.DecideTimeoutSeconds < 3 || req.DecideTimeoutSeconds > 300 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "decide_timeout_seconds 必须在 3~300 之间"})
			return
		}
		settings.DecideTimeoutSeconds = req.DecideTimeoutSeconds
	}
	if req.MaxStatusChars != 0 {
		if req.MaxStatusChars < 200 || req.MaxStatusChars > 8000 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "max_status_chars 必须在 200~8000 之间"})
			return
		}
		settings.MaxStatusChars = req.MaxStatusChars
	}

	settings.ID = npcAgentSettingsRowID
	if err := ac.DB.Save(&settings).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "settings": settings})
}

// defaultNpcAgentSettings 与主服务 npcagent 的默认值保持一致
func defaultNpcAgentSettings() models.NpcAgentSettings {
	return models.NpcAgentSettings{
		ID:                     npcAgentSettingsRowID,
		Enabled:                false, // 默认关闭：开启 NPC 自主行动属于明确动作
		TickSeconds:            1,
		DefaultIntervalSeconds: 30,
		DecideTimeoutSeconds:   25,
		MaxStatusChars:         1200,
	}
}

// loadSettings 读取全局设置；managed=false 表示库里还没有保存过（此时主服务仍按
// main_config.yaml 的 npc_agent 段运行）。刻意不在 GET 时落库：否则"打开一次页面"
// 就会把 yaml 的启用状态悄悄改掉。
func (ac *NpcAgentController) loadSettings() (models.NpcAgentSettings, bool) {
	var settings models.NpcAgentSettings
	err := ac.DB.Where("id = ?", npcAgentSettingsRowID).First(&settings).Error
	if err == nil {
		return settings, true
	}
	return defaultNpcAgentSettings(), false
}

// ── 主服务拉取（内部接口） ──

// GetNpcAgentsInternal GET /api/internal/npc-agents
// 主服务据此覆盖 yaml 配置；只返回启用状态的 agent，避免主服务还要再判一次
func (ac *NpcAgentController) GetNpcAgentsInternal(c *gin.Context) {
	var rows []models.NpcAgentConfig
	if err := ac.DB.Order("agent_id asc").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "读取配置失败: " + err.Error()})
		return
	}
	settings, settingsManaged := ac.loadSettings()

	agents := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		agents = append(agents, gin.H{
			"id":               row.AgentID,
			"name":             row.Name,
			"persona":          row.Persona,
			"interval_seconds": row.IntervalSeconds,
			"enabled":          row.Enabled,
			"allowed_actions":  splitNpcAgentActions(row.AllowedActions),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		// settings_managed=false → 主服务不要用这份默认值覆盖 yaml
		"settings_managed": settingsManaged,
		"settings": gin.H{
			"enabled":                  settings.Enabled,
			"tick_seconds":             settings.TickSeconds,
			"default_interval_seconds": settings.DefaultIntervalSeconds,
			"decide_timeout_seconds":   settings.DecideTimeoutSeconds,
			"max_status_chars":         settings.MaxStatusChars,
		},
		// agents_managed=false → 库里一条都没配，主服务继续用 yaml 的 agents
		"agents_managed": len(rows) > 0,
		"agents":         agents,
	})
}

// ── 主服务状态代理 ──

// GetNpcAgentStatus GET /api/admin/npc-agents/status
func (ac *NpcAgentController) GetNpcAgentStatus(c *gin.Context) {
	c.JSON(http.StatusOK, ac.fetchRuntimeStatus())
}

// fetchRuntimeStatus 拉取主服务只读状态；失败也返回 200 + available=false，
// 让前端能区分"调度没开"和"主服务连不上"，而不是页面直接报错。
func (ac *NpcAgentController) fetchRuntimeStatus() gin.H {
	url := ac.MainServiceURL + "/internal/npc-agents/status"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return gin.H{"available": false, "error": "构造状态请求失败: " + err.Error()}
	}
	if ac.InternalAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+ac.InternalAuthToken)
	}
	client := &http.Client{Timeout: npcAgentStatusTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return gin.H{"available": false, "error": "主服务不可达: " + err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return gin.H{"available": false, "error": "读取主服务响应失败: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return gin.H{"available": false, "error": fmt.Sprintf("主服务返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))}
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return gin.H{"available": false, "error": "主服务响应不是合法 JSON"}
	}
	payload["available"] = true
	return payload
}
