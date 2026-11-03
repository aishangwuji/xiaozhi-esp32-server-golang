package controllers

// npc_agent_test.go — NPC 常驻 Agent 配置点与状态代理的回归
//
// 断言重点（不是"能跑通"，而是"错了会露出来"）：
//   - agent_id 是业务主键：同一 agent 重复提交只会更新，不会插出两行；
//   - 动作白名单与客户端/主服务三处一致，写错的动作必须被拒；
//   - 主服务不可达时状态接口仍返回 200 + available=false，而不是让前端整页报错。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func setupNpcAgentTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:npc_agent_test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.NpcAgentConfig{}, &models.NpcAgentSettings{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	// 共享内存库在同一进程内复用，先清干净再播种
	if err := db.Exec("DELETE FROM npc_agent_config").Error; err != nil {
		t.Fatalf("清理 npc_agent_config: %v", err)
	}
	if err := db.Exec("DELETE FROM npc_agent_settings").Error; err != nil {
		t.Fatalf("清理 npc_agent_settings: %v", err)
	}
	return db
}

func newNpcAgentTestRouter(ctrl *NpcAgentController) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/npc-agents", ctrl.GetNpcAgents)
	r.POST("/api/admin/npc-agents", ctrl.UpsertNpcAgent)
	r.DELETE("/api/admin/npc-agents/:id", ctrl.DeleteNpcAgent)
	r.GET("/api/admin/npc-agents/settings", ctrl.GetNpcAgentSettings)
	r.PUT("/api/admin/npc-agents/settings", ctrl.UpdateNpcAgentSettings)
	r.GET("/api/internal/npc-agents", ctrl.GetNpcAgentsInternal)
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		buf.Write(raw)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ── 边界校验 ──

func TestValidateNpcAgentInput(t *testing.T) {
	disabled := false
	cases := []struct {
		name     string
		req      npcAgentRequest
		wantErr  string
		wantAct  string // 规范化后的 allowed_actions
		wantOn   bool
		wantSecs int
	}{
		{
			name:     "合法_最小字段",
			req:      npcAgentRequest{AgentID: " bob-agent "},
			wantAct:  "",
			wantOn:   true, // 未传 enabled 视为启用
			wantSecs: 30,   // 未传周期取默认
		},
		{
			name:     "显式关闭",
			req:      npcAgentRequest{AgentID: "bob-agent", Enabled: &disabled},
			wantOn:   false,
			wantSecs: 30,
		},
		{
			name:    "agent_id 必填",
			req:     npcAgentRequest{AgentID: "   "},
			wantErr: "agent_id 不能为空",
		},
		{
			name:    "周期过小",
			req:     npcAgentRequest{AgentID: "bob-agent", IntervalSeconds: 1},
			wantErr: "interval_seconds 必须在",
		},
		{
			name:    "周期过大",
			req:     npcAgentRequest{AgentID: "bob-agent", IntervalSeconds: 99999},
			wantErr: "interval_seconds 必须在",
		},
		{
			name:    "未知动作",
			req:     npcAgentRequest{AgentID: "bob-agent", AllowedActions: []string{"fly"}},
			wantErr: "未知动作",
		},
		{
			name:     "动作去重与归一化",
			req:      npcAgentRequest{AgentID: "bob-agent", AllowedActions: []string{"Walk", "walk", " wait ", ""}},
			wantAct:  "walk,wait",
			wantOn:   true,
			wantSecs: 30,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, errText := validateNpcAgentInput(&c.req)
			if c.wantErr != "" {
				if !containsSubstring(errText, c.wantErr) {
					t.Fatalf("期望错误包含 %q，实际 %q", c.wantErr, errText)
				}
				return
			}
			if errText != "" {
				t.Fatalf("不应报错: %s", errText)
			}
			if row.AllowedActions != c.wantAct {
				t.Fatalf("allowed_actions = %q，期望 %q", row.AllowedActions, c.wantAct)
			}
			if row.Enabled != c.wantOn {
				t.Fatalf("enabled = %v，期望 %v", row.Enabled, c.wantOn)
			}
			if row.IntervalSeconds != c.wantSecs {
				t.Fatalf("interval = %d，期望 %d", row.IntervalSeconds, c.wantSecs)
			}
		})
	}
}

func TestNpcAgentKnownActionsMatchesClient(t *testing.T) {
	// 与客户端 npc_act 的 enum、主服务 npcagent.DefaultAllowedActions 三处必须一致
	want := []string{"walk", "run", "jump", "talk", "attack", "wait", "stop"}
	if len(npcAgentKnownActions) != len(want) {
		t.Fatalf("动作数不一致: %v", npcAgentKnownActions)
	}
	for _, a := range want {
		if !containsString(npcAgentKnownActions, a) {
			t.Fatalf("缺少动作 %q", a)
		}
	}
}

// ── 写入 / 拉取闭环 ──

func TestNpcAgentUpsertAndInternalPull(t *testing.T) {
	db := setupNpcAgentTestDB(t)
	ctrl := NewNpcAgentController(db, "token")
	r := newNpcAgentTestRouter(ctrl)

	// 1. 新建
	rec := doJSON(t, r, http.MethodPost, "/api/admin/npc-agents", map[string]any{
		"agent_id":         "bob-agent",
		"name":             "铁匠 鲍勃",
		"persona":          "固执的铁匠",
		"interval_seconds": 30,
		"allowed_actions":  []string{"walk", "wait"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建失败: %d %s", rec.Code, rec.Body.String())
	}

	// 2. 同一 agent_id 再提交（不带 id）：必须更新而不是插第二行
	rec = doJSON(t, r, http.MethodPost, "/api/admin/npc-agents", map[string]any{
		"agent_id":         "bob-agent",
		"name":             "铁匠 鲍勃（改）",
		"persona":          "改过的人设",
		"interval_seconds": 45,
		"enabled":          false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", rec.Code, rec.Body.String())
	}

	var count int64
	if err := db.Model(&models.NpcAgentConfig{}).Where("agent_id = ?", "bob-agent").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一 agent_id 应只有 1 行，实际 %d", count)
	}

	var row models.NpcAgentConfig
	if err := db.Where("agent_id = ?", "bob-agent").First(&row).Error; err != nil {
		t.Fatalf("读取: %v", err)
	}
	if row.Name != "铁匠 鲍勃（改）" || row.IntervalSeconds != 45 || row.Enabled {
		t.Fatalf("更新未生效: %+v", row)
	}
	if row.AllowedActions != "" {
		t.Fatalf("未传 allowed_actions 应被置空（表示默认全集），实际 %q", row.AllowedActions)
	}

	// 3. 主服务拉取：设置 + agent 列表，字段名必须与 internal 契约一致
	rec = doJSON(t, r, http.MethodGet, "/api/internal/npc-agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("内部拉取失败: %d", rec.Code)
	}
	var pull struct {
		SettingsManaged bool `json:"settings_managed"`
		AgentsManaged   bool `json:"agents_managed"`
		Settings        struct {
			Enabled                bool `json:"enabled"`
			TickSeconds            int  `json:"tick_seconds"`
			DefaultIntervalSeconds int  `json:"default_interval_seconds"`
			DecideTimeoutSeconds   int  `json:"decide_timeout_seconds"`
			MaxStatusChars         int  `json:"max_status_chars"`
		} `json:"settings"`
		Agents []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Persona        string   `json:"persona"`
			IntervalSecond int      `json:"interval_seconds"`
			Enabled        bool     `json:"enabled"`
			AllowedActions []string `json:"allowed_actions"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pull); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pull.Agents) != 1 || pull.Agents[0].ID != "bob-agent" {
		t.Fatalf("内部契约的 agent id 字段应为 id，实际 %+v", pull.Agents)
	}
	if pull.Settings.TickSeconds != 1 || pull.Settings.DefaultIntervalSeconds != 30 {
		t.Fatalf("默认设置不对: %+v", pull.Settings)
	}
	// 库里已有一行 agent 配置 → agents 由库接管；但全局设置从未保存过 → 仍应让 yaml 说话。
	// 这条是"打开一次页面就把 yaml 的启用状态悄悄改掉"这个坑的护栏。
	if !pull.AgentsManaged {
		t.Fatal("库里已有 agent 配置时 agents_managed 应为 true")
	}
	if pull.SettingsManaged {
		t.Fatal("从未保存过全局设置时 settings_managed 应为 false，否则会覆盖 yaml 的启用状态")
	}

	// 4. 删除
	rec = doJSON(t, r, http.MethodDelete, "/api/admin/npc-agents/1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败: %d %s", rec.Code, rec.Body.String())
	}
	if err := db.Model(&models.NpcAgentConfig{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("删除后应为 0 行，实际 %d", count)
	}
}

// ── 全局设置 ──

func TestNpcAgentSettingsDefaultsAndValidation(t *testing.T) {
	db := setupNpcAgentTestDB(t)
	ctrl := NewNpcAgentController(db, "token")
	r := newNpcAgentTestRouter(ctrl)

	// 首次读取：自动落默认行，且默认关闭（开启是明确动作）
	rec := doJSON(t, r, http.MethodGet, "/api/admin/npc-agents/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置失败: %d", rec.Code)
	}
	var got struct {
		Settings models.NpcAgentSettings `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Settings.Enabled {
		t.Fatal("调度总开关默认应为关闭")
	}
	if got.Settings.TickSeconds != 1 {
		t.Fatalf("tick 默认应为 1，实际 %d", got.Settings.TickSeconds)
	}
	// 只读不落库：否则打开一次页面就创建了一行默认设置，把 yaml 的配置顶掉
	var rows int64
	if err := db.Model(&models.NpcAgentSettings{}).Count(&rows).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Fatalf("GET 不应写库，实际已有 %d 行", rows)
	}

	// 越界必须拒绝
	rec = doJSON(t, r, http.MethodPut, "/api/admin/npc-agents/settings", map[string]any{"tick_seconds": 100})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界 tick 应返回 400，实际 %d", rec.Code)
	}

	// 合法更新并持久化
	enabled := true
	rec = doJSON(t, r, http.MethodPut, "/api/admin/npc-agents/settings", map[string]any{
		"enabled":                  enabled,
		"tick_seconds":             2,
		"default_interval_seconds": 60,
		"decide_timeout_seconds":   20,
		"max_status_chars":         800,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("更新设置失败: %d %s", rec.Code, rec.Body.String())
	}
	var saved models.NpcAgentSettings
	if err := db.Where("id = ?", npcAgentSettingsRowID).First(&saved).Error; err != nil {
		t.Fatalf("读取持久化设置: %v", err)
	}
	if !saved.Enabled || saved.TickSeconds != 2 || saved.DefaultIntervalSeconds != 60 || saved.MaxStatusChars != 800 {
		t.Fatalf("设置未持久化: %+v", saved)
	}
}

// ── 状态代理 ──

func TestNpcAgentStatusProxyFallback(t *testing.T) {
	db := setupNpcAgentTestDB(t)
	ctrl := NewNpcAgentController(db, "token")
	// 指向一个必然拒绝连接的地址：接口必须降级返回 200 + available=false，而不是 500/超时
	ctrl.MainServiceURL = "http://127.0.0.1:1"

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/admin/npc-agents/status", ctrl.GetNpcAgentStatus)

	rec := doJSON(t, r, http.MethodGet, "/api/admin/npc-agents/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("主服务不可达时应降级为 200，实际 %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if available, _ := payload["available"].(bool); available {
		t.Fatalf("不应声称可用: %+v", payload)
	}
	if payload["error"] == nil || payload["error"] == "" {
		t.Fatalf("应带上不可达原因: %+v", payload)
	}
}

func TestNpcAgentListIncludesRuntimeFallback(t *testing.T) {
	db := setupNpcAgentTestDB(t)
	ctrl := NewNpcAgentController(db, "token")
	ctrl.MainServiceURL = "http://127.0.0.1:1"
	r := newNpcAgentTestRouter(ctrl)

	rec := doJSON(t, r, http.MethodGet, "/api/admin/npc-agents", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表接口应始终可用，实际 %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Agents  []any          `json:"agents"`
		Runtime map[string]any `json:"runtime"`
		Actions []string       `json:"known_actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Runtime == nil {
		t.Fatal("必须带 runtime 字段，否则前端无法说明为什么没在动")
	}
	if available, _ := payload.Runtime["available"].(bool); available {
		t.Fatal("主服务不可达时 runtime.available 应为 false")
	}
	if len(payload.Actions) != len(npcAgentKnownActions) {
		t.Fatalf("known_actions 应透出给前端做下拉: %v", payload.Actions)
	}
}

func containsSubstring(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && bytes.Contains([]byte(haystack), []byte(needle)))
}
