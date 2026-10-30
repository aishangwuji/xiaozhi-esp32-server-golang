package controllers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// webSearchProbeBaseURL DeepSeek 默认 API 地址
const webSearchProbeBaseURL = "https://api.deepseek.com"

// TestWebSearchConnection 探测 DeepSeek Search 接口是否可用（仅用传入参数，不落库、不影响线上配置）
// 请求：{ "base_url": "https://api.deepseek.com", "api_key": "sk-..." }
// 返回：{ "data": { "ok": true, "results": 1 } } 或 { "error": "..." }
func (ac *AdminController) TestWebSearchConnection(c *gin.Context) {
	var req struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数格式错误"})
		return
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先填写 API Key"})
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if baseURL == "" {
		baseURL = webSearchProbeBaseURL
	}

	payload, err := json.Marshal(map[string]interface{}{
		"query":        "DeepSeek",
		"count":        1,
		"need_content": false,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "构造探测请求失败"})
		return
	}

	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/tools/search", bytes.NewReader(payload))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Base URL 不合法: " + err.Error()})
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": false, "error": "网络请求失败: " + err.Error()}})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusOK {
		var parsed struct {
			Results []json.RawMessage `json:"results"`
		}
		_ = json.Unmarshal(body, &parsed)
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true, "results": len(parsed.Results)}})
		return
	}

	// 尽力解析 DeepSeek 的 OpenAI 兼容错误结构
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := string(body)
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		msg = apiErr.Error.Message
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}

	hint := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		hint = "API Key 无效或已失效"
	case http.StatusPaymentRequired:
		hint = "账号余额不足，请先充值"
	case http.StatusTooManyRequests:
		hint = "触发限速（QPS），稍后重试"
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"ok":          false,
		"status_code": resp.StatusCode,
		"error":       msg,
		"hint":        hint,
	}})
}
