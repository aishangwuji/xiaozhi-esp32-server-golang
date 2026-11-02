package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xiaozhi/manager/backend/models"
	"xiaozhi/manager/backend/services/toolcatalog"

	"github.com/gin-gonic/gin"
)

// TestGetNpcToolManifestOverview 校验目录总览的合并语义：
//   - 全局条目与 agent 专属条目按 agent 优先预合并
//   - enabled=false 反映为最终不可见
//   - 描述被覆盖时 desc_overridden=true
//   - 清单里写错的工具名进入 unmatched_entries（主服务对未命中是静默的，这里必须报出来）
func TestGetNpcToolManifestOverview(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupAgentDeviceServiceTestDB(t)
	if err := db.AutoMigrate(&models.NpcToolManifest{}); err != nil {
		t.Fatalf("migrate npc_tool_manifest: %v", err)
	}
	// 测试库是 file::memory:?cache=shared，同进程内共享，必须先清干净再播种
	if err := db.Exec("DELETE FROM npc_tool_manifest").Error; err != nil {
		t.Fatalf("清理 npc_tool_manifest: %v", err)
	}
	rows := []models.NpcToolManifest{
		{AgentID: "", ToolName: "web_search", Description: "全局统一的联网搜索描述", Enabled: true, Remark: "种子"},
		{AgentID: "", ToolName: "self.server.exec", Enabled: false, Remark: "高危：禁止执行命令"},
		{AgentID: "7", ToolName: "self.net.ping", Enabled: false, Remark: "该 NPC 不需要网络探测"},
		{AgentID: "", ToolName: "npc_trad", Enabled: true, Remark: "手打错字，应当报未命中"},
	}
	for i := range rows {
		want := rows[i].Enabled
		rows[i].Enabled = true // 先按默认值创建，避免被 default:true 顶掉
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create manifest row %s: %v", rows[i].ToolName, err)
		}
		if !want {
			if err := db.Model(&models.NpcToolManifest{}).Where("id = ?", rows[i].ID).Update("enabled", false).Error; err != nil {
				t.Fatalf("set enabled=false for %s: %v", rows[i].ToolName, err)
			}
		}
	}

	router := gin.New()
	adminController := &AdminController{DB: db}
	router.GET("/api/admin/npc-tools/overview", adminController.GetNpcToolManifestOverview)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/admin/npc-tools/overview?agent_id=7", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		Data struct {
			Totals struct {
				Tools      int `json:"tools"`
				Declared   int `json:"declared"`
				InManifest int `json:"in_manifest"`
				Entries    int `json:"entries"`
				Unlisted   int `json:"unlisted"`
				Disabled   int `json:"disabled"`
				Unmatched  int `json:"unmatched"`
			} `json:"totals"`
			Tools            []npcToolOverviewItem   `json:"tools"`
			UnmatchedEntries []npcToolUnmatchedEntry `json:"unmatched_entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	catalog, err := toolcatalog.Get()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if payload.Data.Totals.Tools != len(catalog.Tools) {
		t.Fatalf("totals.tools = %d, want %d（声明式目录条数）", payload.Data.Totals.Tools, len(catalog.Tools))
	}

	// 3 条命中（web_search / self.server.exec / self.net.ping），1 条打错字
	if payload.Data.Totals.InManifest != 3 {
		t.Fatalf("totals.in_manifest = %d, want 3", payload.Data.Totals.InManifest)
	}
	if payload.Data.Totals.Unlisted != len(catalog.Tools)-3 {
		t.Fatalf("totals.unlisted = %d, want %d", payload.Data.Totals.Unlisted, len(catalog.Tools)-3)
	}
	if payload.Data.Totals.Unmatched != 1 {
		t.Fatalf("totals.unmatched = %d, want 1", payload.Data.Totals.Unmatched)
	}
	if payload.Data.Totals.Disabled != 2 {
		t.Fatalf("totals.disabled = %d, want 2（全局 self.server.exec + agent 专属 self.net.ping）", payload.Data.Totals.Disabled)
	}

	byName := make(map[string]npcToolOverviewItem, len(payload.Data.Tools))
	for _, it := range payload.Data.Tools {
		byName[it.Name] = it
	}

	search, ok := byName["web_search"]
	if !ok {
		t.Fatalf("工具总览里缺少 web_search")
	}
	if !search.InManifest || !search.Enabled || !search.DescOverridden {
		t.Fatalf("web_search 状态不对: %+v", search)
	}
	if search.ManifestScope != "global" {
		t.Fatalf("web_search manifest_scope = %q, want global", search.ManifestScope)
	}

	exec, ok := byName["self.server.exec"]
	if !ok {
		t.Fatalf("工具总览里缺少 self.server.exec")
	}
	if exec.Enabled {
		t.Fatalf("self.server.exec 应为已摘除状态: %+v", exec)
	}

	// agent 专属条目应覆盖全局语义：self.net.ping 只有 agent 7 的条目
	ping, ok := byName["self.net.ping"]
	if !ok {
		t.Fatalf("工具总览里缺少 self.net.ping")
	}
	if ping.Enabled || ping.ManifestScope != "agent" {
		t.Fatalf("self.net.ping 状态不对: %+v", ping)
	}

	if len(payload.Data.UnmatchedEntries) != 1 || payload.Data.UnmatchedEntries[0].ToolName != "npc_trad" {
		t.Fatalf("unmatched_entries = %+v, want 仅 npc_trad", payload.Data.UnmatchedEntries)
	}
}

// TestGetNpcToolManifestOverviewEmptyDB 没有清单条目时，所有工具都应可见且登记数为 0。
func TestGetNpcToolManifestOverviewEmptyDB(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupAgentDeviceServiceTestDB(t)
	if err := db.AutoMigrate(&models.NpcToolManifest{}); err != nil {
		t.Fatalf("migrate npc_tool_manifest: %v", err)
	}
	// 测试库是 file::memory:?cache=shared，同进程内共享，必须先清干净再播种
	if err := db.Exec("DELETE FROM npc_tool_manifest").Error; err != nil {
		t.Fatalf("清理 npc_tool_manifest: %v", err)
	}
	router := gin.New()
	adminController := &AdminController{DB: db}
	router.GET("/api/admin/npc-tools/overview", adminController.GetNpcToolManifestOverview)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/npc-tools/overview", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Data struct {
			Totals struct {
				InManifest int `json:"in_manifest"`
				Unlisted   int `json:"unlisted"`
				Disabled   int `json:"disabled"`
			} `json:"totals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Data.Totals.InManifest != 0 || payload.Data.Totals.Disabled != 0 {
		t.Fatalf("空清单下 in_manifest/disabled 应为 0，实际 %+v", payload.Data.Totals)
	}
	if payload.Data.Totals.Unlisted == 0 {
		t.Fatalf("空清单下所有工具都应算未登记，实际 unlisted=0")
	}
}

// TestUpsertNpcToolManifestPersistsDisabled 回归测试：
// Enabled 字段带 gorm default:true，bool 零值在 Create 时会被列默认值顶掉，
// 曾经导致"控制台新增一条 enabled=false 的条目"实际存成 true（摘除失效）。
func TestUpsertNpcToolManifestPersistsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupAgentDeviceServiceTestDB(t)
	if err := db.AutoMigrate(&models.NpcToolManifest{}); err != nil {
		t.Fatalf("migrate npc_tool_manifest: %v", err)
	}
	if err := db.Exec("DELETE FROM npc_tool_manifest").Error; err != nil {
		t.Fatalf("清理 npc_tool_manifest: %v", err)
	}

	router := gin.New()
	adminController := &AdminController{DB: db}
	router.POST("/api/admin/npc-tools", adminController.UpsertNpcToolManifest)

	body := `{"agent_id":"","tool_name":"self.server.exec","description":"","input_schema":"","enabled":false,"remark":"高危摘除"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/npc-tools", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", recorder.Code, recorder.Body.String())
	}

	var row models.NpcToolManifest
	if err := db.Where("agent_id = ? AND tool_name = ?", "", "self.server.exec").First(&row).Error; err != nil {
		t.Fatalf("query row: %v", err)
	}
	if row.Enabled {
		t.Fatalf("新增 enabled=false 的条目被存成了 true（GORM default 顶掉零值）")
	}

	// 再改回启用，走更新分支，确认能翻回来
	body = `{"agent_id":"","tool_name":"self.server.exec","description":"","input_schema":"","enabled":true,"remark":"恢复"}`
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/admin/npc-tools", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", recorder.Code, recorder.Body.String())
	}

	row = models.NpcToolManifest{}
	if err := db.Where("agent_id = ? AND tool_name = ?", "", "self.server.exec").First(&row).Error; err != nil {
		t.Fatalf("query row after enable: %v", err)
	}
	if !row.Enabled {
		t.Fatalf("重新启用失败，enabled 仍为 false")
	}
}

// TestGetNpcToolManifestOverviewDeviceWithoutController 传了 device_id 但控制器不可用时，
// 必须给出明确说明而不是静默返回空列表（否则会被误读成"设备在线但没有工具"）。
func TestGetNpcToolManifestOverviewDeviceWithoutController(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupAgentDeviceServiceTestDB(t)
	if err := db.AutoMigrate(&models.NpcToolManifest{}); err != nil {
		t.Fatalf("migrate npc_tool_manifest: %v", err)
	}

	router := gin.New()
	adminController := &AdminController{DB: db} // WebSocketController 故意留空
	router.GET("/api/admin/npc-tools/overview", adminController.GetNpcToolManifestOverview)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/npc-tools/overview?device_id=4", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var payload struct {
		Data struct {
			DeviceNote string `json:"device_note"`
			Totals     struct {
				Live int `json:"live"`
			} `json:"totals"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Data.DeviceNote == "" {
		t.Fatalf("device_id 传了但控制器不可用时 device_note 不能为空")
	}
	if payload.Data.Totals.Live != 0 {
		t.Fatalf("控制器不可用时 live 应为 0，实际 %d", payload.Data.Totals.Live)
	}
}
