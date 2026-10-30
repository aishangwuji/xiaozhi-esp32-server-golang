package websearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/viper"
)

func setupTestServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	viper.Set("websearch.deepseek.base_url", ts.URL)
	viper.Set("websearch.deepseek.api_key", "test-key")
	viper.Set("websearch.deepseek.timeout_seconds", 5)
}

func resetViper(t *testing.T) {
	t.Helper()
	for _, k := range []string{"websearch.deepseek.base_url", "websearch.deepseek.api_key", "websearch.deepseek.timeout_seconds"} {
		viper.Set(k, nil)
	}
}

func TestSearchSuccess(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != searchEndpoint {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("鉴权头错误: %s", r.Header.Get("Authorization"))
		}
		var req searchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("请求体解析失败: %v", err)
		}
		if req.Query != "测试" || req.Count != 3 || !req.NeedContent {
			t.Errorf("请求参数错误: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"rerank_score": 2.4,
					"title":        "测试结果",
					"url":          "https://example.com/a",
					"snippet":      "短摘要",
					"summary":      "长摘要",
					"content":      "正文内容",
					"authority":    3,
				},
			},
		})
	})

	results, err := Search(context.Background(), "测试", 3, true, "")
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(results) != 1 || results[0].Title != "测试结果" || results[0].URL != "https://example.com/a" {
		t.Errorf("结果解析错误: %+v", results)
	}
}

func TestSearchEmptyResults(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results": []}`))
	})
	results, err := Search(context.Background(), "无结果", 5, false, "")
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("期望空结果，实际: %+v", results)
	}
}

func TestSearchAPIError(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"'count' must be between 1 and 50","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`))
	})
	_, err := Search(context.Background(), "测试", 3, false, "")
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
}

func TestSearchInvalidParams(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求")
	})
	if _, err := Search(context.Background(), "  ", 3, false, ""); err == nil {
		t.Error("空 query 应报错")
	}
	long := make([]rune, maxQueryLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := Search(context.Background(), string(long), 3, false, ""); err == nil {
		t.Error("超长 query 应报错")
	}
}

func TestFetchSuccess(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fetchEndpoint {
			t.Errorf("路径错误: %s", r.URL.Path)
		}
		var req fetchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("请求体解析失败: %v", err)
		}
		if req.URL != "https://example.com/article" {
			t.Errorf("URL 参数错误: %s", req.URL)
		}
		_, _ = w.Write([]byte(`{"title":"示例文章","content":"# 示例文章\n\n正文。"}`))
	})

	page, err := Fetch(context.Background(), "https://example.com/article")
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if page.Title != "示例文章" || page.Content == "" {
		t.Errorf("结果解析错误: %+v", page)
	}
}

func TestFetchInvalidURL(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求")
	})
	cases := []string{
		"",
		"ftp://example.com/a",            // 非 http/https
		"example.com/no-host",            // 无 host
		"https://user:pass@example.com/", // 带用户名密码
	}
	for _, u := range cases {
		if _, err := Fetch(context.Background(), u); err == nil {
			t.Errorf("URL %q 应报错", u)
		}
	}
}

func TestNotConfigured(t *testing.T) {
	defer resetViper(t)
	setupTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应发出请求")
	})
	viper.Set("websearch.deepseek.api_key", "")
	if IsConfigured() {
		t.Error("未配置 api_key 时 IsConfigured 应为 false")
	}
	if _, err := Search(context.Background(), "测试", 3, false, ""); err == nil {
		t.Error("未配置 api_key 时应报错")
	}
}
