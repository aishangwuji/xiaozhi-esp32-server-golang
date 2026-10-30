package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"xiaozhi-esp32-server-golang/internal/domain/websearch"
	log "xiaozhi-esp32-server-golang/logger"

	"github.com/spf13/viper"
)

// InitWebSearchMCPTools 初始化联网搜索相关的本地MCP工具（web_search / web_fetch），
// 在 InitChatLocalMCPTools 中被调用。
// 未启用（websearch.enabled=false）或未配置 API Key 时不注册，避免给 LLM 暴露不可用工具。
func InitWebSearchMCPTools() {
	if !websearch.IsEnabled() {
		log.Info("websearch 未启用，跳过注册 web_search/web_fetch 工具")
		return
	}
	if !websearch.IsConfigured() {
		log.Warn("未配置 websearch.deepseek.api_key，跳过注册 web_search/web_fetch 工具")
		return
	}

	if err := RegisterLocalMcpFunc(
		"web_search",
		"联网搜索工具。当用户询问实时信息、新闻、天气、体育赛果、价格、版本发布等需要联网查询的最新内容，或你的知识可能过时时使用。支持 site:/-site:/\"精确短语\"/lang:/after:/before: 等搜索语法组合。返回每条结果的标题、链接与摘要",
		WebSearchParams{},
		webSearchHandler,
	); err != nil {
		log.Errorf("注册 web_search 工具失败: %v", err)
		return
	}

	if err := RegisterLocalMcpFunc(
		"web_fetch",
		"网页抓取工具。抓取指定URL的网页并返回 Markdown 正文。通常在 web_search 的摘要信息不够、需要阅读某个结果全文时使用",
		WebFetchParams{},
		webFetchHandler,
	); err != nil {
		log.Errorf("注册 web_fetch 工具失败: %v", err)
	}
}

// WebSearchParams web_search 工具入参
type WebSearchParams struct {
	Query       string `json:"query" description:"搜索关键词，1~128个字符，可组合 site:/lang:/after: 等搜索语法" required:"true"`
	Count       int    `json:"count,omitempty" description:"返回结果条数，1~50，默认5"`
	OriQuestion string `json:"ori_question,omitempty" description:"触发搜索的用户原始问题，有助于理解搜索意图"`
}

// WebFetchParams web_fetch 工具入参
type WebFetchParams struct {
	URL string `json:"url" description:"要抓取的网页地址（http/https）" required:"true"`
}

func webSearchHandler(ctx context.Context, argumentsInJSON string) (string, error) {
	log.Info("执行联网搜索工具 web_search")

	var params WebSearchParams
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &params); err != nil {
			response := NewErrorResponse("web_search", "参数解析失败", "PARSE_ERROR", "请检查 query 参数格式")
			return response.ToJSON()
		}
	}
	params.Query = strings.TrimSpace(params.Query)
	if params.Query == "" {
		response := NewErrorResponse("web_search", "query 不能为空", "INVALID_QUERY", "请提供要搜索的内容")
		return response.ToJSON()
	}
	if params.Count <= 0 {
		params.Count = 5
	}

	results, err := websearch.Search(ctx, params.Query, params.Count, true, params.OriQuestion)
	if err != nil {
		log.Errorf("web_search 搜索失败: %v", err)
		response := NewErrorResponse("web_search", fmt.Sprintf("联网搜索失败: %v", err), "SEARCH_FAILED", "请稍后重试或更换搜索词")
		return response.ToJSON()
	}

	data := map[string]interface{}{
		"query":   params.Query,
		"count":   len(results),
		"results": results,
	}

	if len(results) == 0 {
		response := NewContentResponse("web_search", data, "没有搜索到相关结果")
		return response.ToJSON()
	}

	// 给 LLM 的简短提示：列出前几条标题
	var builder strings.Builder
	limit := len(results)
	if limit > 3 {
		limit = 3
	}
	for i := 0; i < limit; i++ {
		builder.WriteString(fmt.Sprintf("%d. %s\n", i+1, results[i].Title))
	}
	msg := fmt.Sprintf("已搜索到 %d 条结果：\n%s（详情见 results 字段）", len(results), strings.TrimSpace(builder.String()))
	response := NewContentResponse("web_search", data, msg)
	return response.ToJSON()
}

func webFetchHandler(ctx context.Context, argumentsInJSON string) (string, error) {
	log.Info("执行网页抓取工具 web_fetch")

	var params WebFetchParams
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &params); err != nil {
			response := NewErrorResponse("web_fetch", "参数解析失败", "PARSE_ERROR", "请检查 url 参数格式")
			return response.ToJSON()
		}
	}
	params.URL = strings.TrimSpace(params.URL)
	if params.URL == "" {
		response := NewErrorResponse("web_fetch", "url 不能为空", "INVALID_URL", "请提供要抓取的网页地址")
		return response.ToJSON()
	}

	page, err := websearch.Fetch(ctx, params.URL)
	if err != nil {
		log.Errorf("web_fetch 抓取失败: %v", err)
		response := NewErrorResponse("web_fetch", fmt.Sprintf("网页抓取失败: %v", err), "FETCH_FAILED", "请检查URL是否可访问，或稍后重试")
		return response.ToJSON()
	}

	data := map[string]interface{}{
		"url":     params.URL,
		"title":   page.Title,
		"content": page.Content,
	}

	// 给 LLM 的简短提示
	msg := strings.TrimSpace(page.Title)
	if msg == "" {
		msg = "已抓取网页正文（详情见 content 字段）"
	} else {
		msg = "已抓取网页《" + msg + "》正文（详情见 content 字段）"
	}
	response := NewContentResponse("web_fetch", data, msg)
	return response.ToJSON()
}
