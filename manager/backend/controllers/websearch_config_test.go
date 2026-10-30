package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xiaozhi/manager/backend/models"

	"github.com/gin-gonic/gin"
)

// TestGetSystemConfigsIncludesWebSearch 校验 type=websearch 的配置会随系统配置下发，
// 主程序据此把 websearch.deepseek.* 合并进 viper。
func TestGetSystemConfigsIncludesWebSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupAgentDeviceServiceTestDB(t)
	if err := db.Create(&models.Config{
		Type:      "websearch",
		Name:      "联网搜索配置",
		ConfigID:  "websearch_default",
		Provider:  "deepseek",
		IsDefault: true,
		Enabled:   true,
		JsonData:  `{"enabled":true,"deepseek":{"base_url":"https://api.deepseek.com","api_key":"sk-unit-test","timeout_seconds":45}}`,
	}).Error; err != nil {
		t.Fatalf("create websearch config: %v", err)
	}

	router := gin.New()
	adminController := &AdminController{DB: db}
	router.GET("/api/system/configs", adminController.GetSystemConfigs)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/system/configs", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	websearch, ok := payload.Data["websearch"].(map[string]interface{})
	if !ok {
		t.Fatalf("data.websearch 缺失或类型错误: %#v", payload.Data["websearch"])
	}
	if enabled, _ := websearch["enabled"].(bool); !enabled {
		t.Fatalf("websearch.enabled = %#v, want true", websearch["enabled"])
	}
	deepseek, ok := websearch["deepseek"].(map[string]interface{})
	if !ok {
		t.Fatalf("websearch.deepseek 缺失或类型错误: %#v", websearch["deepseek"])
	}
	if deepseek["api_key"] != "sk-unit-test" {
		t.Fatalf("api_key = %#v, want sk-unit-test", deepseek["api_key"])
	}
	if timeout, _ := deepseek["timeout_seconds"].(float64); timeout != 45 {
		t.Fatalf("timeout_seconds = %#v, want 45", deepseek["timeout_seconds"])
	}
}

// TestTestWebSearchConnectionSuccess 用假 DeepSeek 服务校验探测接口的成功分支
func TestTestWebSearchConnectionSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/search" {
			t.Errorf("path = %s, want /v1/tools/search", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-probe" {
			t.Errorf("authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"t","url":"https://example.com"}]}`))
	}))
	defer upstream.Close()

	router := gin.New()
	adminController := &AdminController{}
	router.POST("/api/admin/websearch-configs/test", adminController.TestWebSearchConnection)

	body := `{"base_url":"` + upstream.URL + `","api_key":"sk-probe"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/websearch-configs/test", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data struct {
			OK      bool `json:"ok"`
			Results int  `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Data.OK || payload.Data.Results != 1 {
		t.Fatalf("ok=%v results=%d, want true/1", payload.Data.OK, payload.Data.Results)
	}
}

// TestTestWebSearchConnectionUnauthorized 校验 Key 无效时的错误提示
func TestTestWebSearchConnectionUnauthorized(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Authentication Fails","type":"authentication_error","param":null,"code":"invalid_api_key"}}`))
	}))
	defer upstream.Close()

	router := gin.New()
	adminController := &AdminController{}
	router.POST("/api/admin/websearch-configs/test", adminController.TestWebSearchConnection)

	body := `{"base_url":"` + upstream.URL + `","api_key":"bad-key"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/admin/websearch-configs/test", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	var payload struct {
		Data struct {
			OK   bool   `json:"ok"`
			Hint string `json:"hint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Data.OK {
		t.Fatal("期望 ok=false")
	}
	if payload.Data.Hint == "" {
		t.Fatal("期望给出 hint 提示")
	}
}
