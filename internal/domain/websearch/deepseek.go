// Package websearch 提供联网搜索能力，当前实现为 DeepSeek 自研搜索引擎
// 的 Search / Fetch 原始 JSON API（https://api.deepseek.com）。
//
// 配置（main_config.yaml）：
//
//	websearch:
//	  enabled: true                # 可选，缺省视为开启
//	  deepseek:
//	    base_url: "https://api.deepseek.com"
//	    api_key: "sk-..."          # 必填，未配置时工具不注册
//	    timeout_seconds: 30        # 可选，默认 30
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "xiaozhi-esp32-server-golang/logger"

	"github.com/spf13/viper"
)

const (
	defaultBaseURL = "https://api.deepseek.com"
	searchEndpoint = "/v1/tools/search"
	fetchEndpoint  = "/v1/tools/fetch"
	defaultTimeout = 30 * time.Second

	maxQueryLen   = 128
	maxCount      = 50
	maxURLLen     = 8192
	maxRetryTimes = 1 // 408/429/5xx 时额外重试一次
)

// SearchResult DeepSeek Search 接口返回的单条结果
type SearchResult struct {
	RerankScore float64 `json:"rerank_score"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Snippet     string  `json:"snippet"`
	Summary     string  `json:"summary"`
	Content     string  `json:"content"`
	SiteName    *string `json:"site_name"`
	SiteIcon    *string `json:"site_icon"`
	Date        *string `json:"date"`
	Authority   int     `json:"authority"`
}

// FetchResult DeepSeek Fetch 接口返回的页面内容
type FetchResult struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

type searchRequest struct {
	Query        string `json:"query"`
	Count        int    `json:"count,omitempty"`
	OriQuestion  string `json:"ori_question,omitempty"`
	Localization string `json:"localization,omitempty"`
	NeedContent  bool   `json:"need_content"`
}

type searchResponse struct {
	Results []SearchResult `json:"results"`
}

type fetchRequest struct {
	URL string `json:"url"`
}

// apiError DeepSeek 的 OpenAI 兼容错误格式
type apiError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

var (
	clientMu      sync.Mutex
	httpClient    *http.Client
	clientTimeout time.Duration
)

// getHTTPClient 返回共享的 HTTP client；超时配置变更时自动重建，便于配置热更
func getHTTPClient() *http.Client {
	timeout := defaultTimeout
	if n := viper.GetInt("websearch.deepseek.timeout_seconds"); n > 0 {
		timeout = time.Duration(n) * time.Second
	}

	clientMu.Lock()
	defer clientMu.Unlock()
	if httpClient == nil || clientTimeout != timeout {
		httpClient = &http.Client{Timeout: timeout}
		clientTimeout = timeout
	}
	return httpClient
}

func getBaseURL() string {
	if u := strings.TrimRight(viper.GetString("websearch.deepseek.base_url"), "/"); u != "" {
		return u
	}
	return defaultBaseURL
}

func getAPIKey() string {
	return strings.TrimSpace(viper.GetString("websearch.deepseek.api_key"))
}

// IsEnabled 判断 websearch 是否启用（配置缺省视为开启）
func IsEnabled() bool {
	if viper.IsSet("websearch.enabled") {
		return viper.GetBool("websearch.enabled")
	}
	return true
}

// IsConfigured 判断是否已配置 API Key（未配置则工具不可用）
func IsConfigured() bool {
	return getAPIKey() != ""
}

// Search 调用 DeepSeek Search 接口按关键词搜索网页。
// query: 搜索词（1~128 字符，支持 site:/lang:/after: 等搜索语法）
// count: 返回条数（1~50，传 0 或负数使用默认值 10）
// needContent: 是否返回网页正文
// oriQuestion: 触发搜索的用户原始问题（可选，帮助理解搜索意图）
func Search(ctx context.Context, query string, count int, needContent bool, oriQuestion string) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	if len([]rune(query)) > maxQueryLen {
		return nil, fmt.Errorf("query 超过 %d 个字符", maxQueryLen)
	}
	if count <= 0 {
		count = 10
	}
	if count > maxCount {
		count = maxCount
	}

	reqBody := searchRequest{
		Query:       query,
		Count:       count,
		NeedContent: needContent,
		OriQuestion: strings.TrimSpace(oriQuestion),
	}

	var resp searchResponse
	if err := doJSON(ctx, http.MethodPost, searchEndpoint, reqBody, &resp); err != nil {
		return nil, fmt.Errorf("DeepSeek Search 请求失败: %w", err)
	}
	return resp.Results, nil
}

// Fetch 调用 DeepSeek Fetch 接口抓取单个网页，返回 Markdown 正文
func Fetch(ctx context.Context, pageURL string) (*FetchResult, error) {
	pageURL = strings.TrimSpace(pageURL)
	if pageURL == "" {
		return nil, fmt.Errorf("url 不能为空")
	}
	if len(pageURL) > maxURLLen {
		return nil, fmt.Errorf("url 超过 %d 个字符", maxURLLen)
	}
	u, err := url.Parse(pageURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("url 不合法: %s", pageURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("仅支持 http/https URL: %s", pageURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("url 不能携带用户名或密码")
	}

	var resp FetchResult
	if err := doJSON(ctx, http.MethodPost, fetchEndpoint, fetchRequest{URL: pageURL}, &resp); err != nil {
		return nil, fmt.Errorf("DeepSeek Fetch 请求失败: %w", err)
	}
	return &resp, nil
}

// doJSON 发送 JSON POST 请求并解析响应；408/429/5xx 额外重试一次
func doJSON(ctx context.Context, method, endpoint string, reqBody, respBody any) error {
	apiKey := getAPIKey()
	if apiKey == "" {
		return fmt.Errorf("未配置 websearch.deepseek.api_key")
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetryTimes; attempt++ {
		if attempt > 0 {
			// 重试前先退避，避免立刻再次触发限速
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, getBaseURL()+endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := getHTTPClient().Do(req)
		if err != nil {
			return fmt.Errorf("网络请求失败: %w", err)
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 响应体上限 10MB
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("读取响应失败: %w", err)
		}

		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(data, respBody); err != nil {
				return fmt.Errorf("解析响应失败: %w", err)
			}
			return nil
		}

		lastErr = buildAPIError(resp.StatusCode, data)

		// 仅对可重试状态码重试
		if resp.StatusCode != http.StatusRequestTimeout &&
			resp.StatusCode != http.StatusTooManyRequests &&
			resp.StatusCode < 500 {
			break
		}
		log.Warnf("DeepSeek %s 返回 %d，attempt=%d: %v", endpoint, resp.StatusCode, attempt+1, lastErr)
	}
	return lastErr
}

func buildAPIError(statusCode int, data []byte) error {
	var apiErr apiError
	if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Error.Message != "" {
		return fmt.Errorf("HTTP %d: %s", statusCode, apiErr.Error.Message)
	}
	return fmt.Errorf("HTTP %d: %s", statusCode, string(data))
}
