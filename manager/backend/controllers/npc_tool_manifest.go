package controllers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// NPC 工具清单管理（工具描述服务端化·阶段一）
//
// 数据流：管理端写入本表 → 主服务经 /api/internal/npc-tools 拉取（InternalServiceAuth 保护）
// → 工具聚合时按清单覆盖描述/入参 schema，enabled=false 的工具被摘除。
// 边界校验：所有写入必须通过长度与格式校验；input_schema 必须是 JSON 对象文本。
// 合并语义：agent 专属条目优先于全局条目（agent_id=''），主服务按 tool_name 对齐。

const (
	npcToolMaxAgentIDLen     = 64
	npcToolMaxToolNameLen    = 128
	npcToolMaxDescriptionLen = 4000
	npcToolMaxSchemaLen      = 8192
	npcToolMaxRemarkLen      = 255
)

// npcToolManifestRequest 管理端写入请求体
type npcToolManifestRequest struct {
	AgentID     string `json:"agent_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	InputSchema string `json:"input_schema"`
	Enabled     *bool  `json:"enabled"` // 指针区分"未传"与"传 false"
	Remark      string `json:"remark"`
}

// validateNpcToolManifestInput 边界校验；返回裁剪后的规范化字段
func validateNpcToolManifestInput(req *npcToolManifestRequest) (agentID, toolName, description, inputSchema, remark string, errResp string) {
	agentID = strings.TrimSpace(req.AgentID)
	toolName = strings.TrimSpace(req.ToolName)
	description = strings.TrimSpace(req.Description)
	inputSchema = strings.TrimSpace(req.InputSchema)
	remark = strings.TrimSpace(req.Remark)

	if len(agentID) > npcToolMaxAgentIDLen {
		return "", "", "", "", "", "agent_id 过长（上限 64 字符）"
	}
	if toolName == "" {
		return "", "", "", "", "", "tool_name 不能为空"
	}
	if len(toolName) > npcToolMaxToolNameLen {
		return "", "", "", "", "", "tool_name 过长（上限 128 字符）"
	}
	if len(description) > npcToolMaxDescriptionLen {
		return "", "", "", "", "", "description 过长（上限 4000 字符）"
	}
	if len(remark) > npcToolMaxRemarkLen {
		return "", "", "", "", "", "remark 过长（上限 255 字符）"
	}
	if inputSchema != "" {
		if len(inputSchema) > npcToolMaxSchemaLen {
			return "", "", "", "", "", "input_schema 过长（上限 8192 字符）"
		}
		// 必须是 JSON 对象（openapi3 schema 形态），数组/标量一律拒绝在边界处
		var probe map[string]interface{}
		if err := json.Unmarshal([]byte(inputSchema), &probe); err != nil || probe == nil {
			return "", "", "", "", "", "input_schema 不是合法的 JSON 对象"
		}
	}
	return agentID, toolName, description, inputSchema, remark, ""
}

// GetNpcToolManifests 管理端列表：GET /api/admin/npc-tools?agent_id=
// agent_id 缺省时返回全部（含全局条目），便于控制台一次看全。
func (ac *AdminController) GetNpcToolManifests(c *gin.Context) {
	if ac == nil || ac.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabaseUnavailable.Error()})
		return
	}
	agentID := strings.TrimSpace(c.Query("agent_id"))
	query := ac.DB.Model(&models.NpcToolManifest{})
	if agentID != "" {
		query = query.Where("agent_id = ?", agentID)
	}
	var rows []models.NpcToolManifest
	if err := query.Order("agent_id ASC, tool_name ASC").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询工具清单失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"items": rows}})
}

// UpsertNpcToolManifest 管理端写入：POST /api/admin/npc-tools
// 按 (agent_id, tool_name) 幂等 upsert：存在则更新，不存在则创建。
func (ac *AdminController) UpsertNpcToolManifest(c *gin.Context) {
	if ac == nil || ac.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabaseUnavailable.Error()})
		return
	}
	var req npcToolManifestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体不是合法 JSON"})
		return
	}
	agentID, toolName, description, inputSchema, remark, errMsg := validateNpcToolManifestInput(&req)
	if errMsg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errMsg})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	var row models.NpcToolManifest
	err := ac.DB.Where("agent_id = ? AND tool_name = ?", agentID, toolName).First(&row).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "查询工具清单失败"})
			return
		}
		row = models.NpcToolManifest{
			AgentID:     agentID,
			ToolName:    toolName,
			Description: description,
			InputSchema: inputSchema,
			Enabled:     enabled,
			Remark:      remark,
		}
		if err := ac.DB.Create(&row).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建工具清单条目失败"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": row})
		return
	}
	row.Description = description
	row.InputSchema = inputSchema
	row.Enabled = enabled
	row.Remark = remark
	if err := ac.DB.Save(&row).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新工具清单条目失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": row})
}

// DeleteNpcToolManifest 管理端删除：DELETE /api/admin/npc-tools/:id
func (ac *AdminController) DeleteNpcToolManifest(c *gin.Context) {
	if ac == nil || ac.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabaseUnavailable.Error()})
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的条目 id"})
		return
	}
	result := ac.DB.Delete(&models.NpcToolManifest{}, id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除工具清单条目失败"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "条目不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

// GetNpcToolManifestsInternal 主服务拉取：GET /api/internal/npc-tools?agent_id=
// 返回该 agent 生效的覆盖清单 = 全局条目 + agent 专属条目（同 tool_name 时 agent 专属优先，
// 预合并后交付，主服务无需再实现优先级）。
func (ac *AdminController) GetNpcToolManifestsInternal(c *gin.Context) {
	if ac == nil || ac.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabaseUnavailable.Error()})
		return
	}
	agentID := strings.TrimSpace(c.Query("agent_id"))

	var rows []models.NpcToolManifest
	query := ac.DB.Where("agent_id = ? OR agent_id = ?", agentID, "")
	if err := query.Order("tool_name ASC").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "查询工具清单失败"})
		return
	}

	// agent 专属条目覆盖同名全局条目
	overrides := make([]gin.H, 0, len(rows))
	agentTools := make(map[string]struct{})
	for _, row := range rows {
		if row.AgentID == agentID {
			agentTools[row.ToolName] = struct{}{}
		}
	}
	for _, row := range rows {
		if row.AgentID != agentID {
			if _, taken := agentTools[row.ToolName]; taken {
				continue
			}
		}
		overrides = append(overrides, gin.H{
			"tool_name":    row.ToolName,
			"description":  row.Description,
			"input_schema": row.InputSchema,
			"enabled":      row.Enabled,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"overrides": overrides}})
}
