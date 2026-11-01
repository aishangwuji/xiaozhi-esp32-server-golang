package mcp

// 工具清单云端覆盖（工具描述服务端化·阶段一）
//
// 数据流：manager 控制台 npc_tool_manifest 表
//   → GET {backend}/api/internal/npc-tools?agent_id=X（InternalServiceAuth 保护）
//   → 本文件 TTL 缓存
//   → GetToolsByDeviceIdWithTransport 聚合完成后应用（纯合并逻辑见 toolmanifest 子包）：
//     enabled=false 摘除；description/input_schema 非空时覆盖 Info 元数据。
//
// 红线约束：
//   - 目录级调整：不新增设备侧未上报的工具（客户端 can_offer 仍是执行可达性的唯一裁决）；
//   - 不参与意图判定：覆盖只改变 LLM 看到的工具描述/入参 schema，执行路径原样委托原工具；
//   - 仅基础设施异常（网络失败/超时/5xx）允许沿用上一份有效缓存或空清单——这是基础设施
//     层兜底，不做任何业务层兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	componentshttp "xiaozhi-esp32-server-golang/internal/components/http"
	toolmanifest "xiaozhi-esp32-server-golang/internal/domain/mcp/toolmanifest"
	"xiaozhi-esp32-server-golang/internal/util"
	log "xiaozhi-esp32-server-golang/logger"

	"github.com/cloudwego/eino/components/tool"
)

const (
	// toolManifestCacheTTL 正常缓存时长：目录调优的生效延迟上界
	toolManifestCacheTTL = 30 * time.Second
	// toolManifestFailureTTL 拉取失败的负缓存时长：manager 故障期间防止每次聚合都被超时拖住
	toolManifestFailureTTL = 15 * time.Second
	// toolManifestHTTPTimeout 单次拉取超时
	toolManifestHTTPTimeout = 5 * time.Second
)

// toolManifestEntry 缓存条目；healthy=false 表示负缓存（上次拉取失败）
type toolManifestEntry struct {
	overrides map[string]toolmanifest.ToolOverride
	fetchedAt time.Time
	healthy   bool
}

func (e toolManifestEntry) fresh(now time.Time) bool {
	ttl := toolManifestCacheTTL
	if !e.healthy {
		ttl = toolManifestFailureTTL
	}
	return now.Sub(e.fetchedAt) < ttl
}

var (
	toolManifestMu    sync.Mutex
	toolManifestCache = make(map[string]toolManifestEntry)
	toolManifestHTTP  *componentshttp.ManagerClient
	toolManifestOnce  sync.Once
)

func getToolManifestHTTPClient() *componentshttp.ManagerClient {
	toolManifestOnce.Do(func() {
		toolManifestHTTP = componentshttp.NewManagerClient(componentshttp.ManagerClientConfig{
			BaseURL:    util.GetBackendURL(),
			AuthToken:  util.GetManagerAuthToken(),
			Timeout:    toolManifestHTTPTimeout,
			MaxRetries: 1,
		})
	})
	return toolManifestHTTP
}

// fetchToolOverrides 拉取 agent 生效清单（控制台侧已预合并：agent 专属条目优先于全局条目）
func fetchToolOverrides(agentID string) ([]toolmanifest.ToolOverride, error) {
	raw, err := getToolManifestHTTPClient().DoRequestRaw(context.Background(), componentshttp.RequestOptions{
		Method:      "GET",
		Path:        "/api/internal/npc-tools",
		QueryParams: map[string]string{"agent_id": agentID},
	})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Overrides []toolmanifest.ToolOverride `json:"overrides"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("工具清单响应解析失败: %w", err)
	}
	return resp.Data.Overrides, nil
}

// getToolOverrides 带缓存的清单获取；失败时沿旧缓存续命或负缓存退避
func getToolOverrides(agentID string) map[string]toolmanifest.ToolOverride {
	now := time.Now()
	toolManifestMu.Lock()
	entry, has := toolManifestCache[agentID]
	toolManifestMu.Unlock()

	if has && entry.fresh(now) {
		if entry.healthy {
			return entry.overrides
		}
		return nil
	}

	overrides, err := fetchToolOverrides(agentID)
	switch {
	case err == nil:
		entry = toolManifestEntry{overrides: toolmanifest.IndexOverrides(overrides), fetchedAt: now, healthy: true}
	case has && entry.healthy:
		// 基础设施异常：上一份有效缓存续命一个 TTL，避免持续故障期间目录闪变
		log.Warnf("拉取 NPC 工具清单失败(沿用上一份有效缓存): agent=%s err=%v", agentID, err)
		entry.fetchedAt = now
	default:
		// 无可用缓存：负缓存短退避
		log.Warnf("拉取 NPC 工具清单失败(本次聚合不应用覆盖): agent=%s err=%v", agentID, err)
		entry = toolManifestEntry{fetchedAt: now, healthy: false}
	}

	toolManifestMu.Lock()
	toolManifestCache[agentID] = entry
	toolManifestMu.Unlock()

	if !entry.healthy {
		return nil
	}
	return entry.overrides
}

// ApplyToolManifestOverrides 工具聚合完成后应用云端清单（agentID 可空：仅全局条目生效）
func ApplyToolManifestOverrides(agentID string, tools map[string]tool.InvokableTool) map[string]tool.InvokableTool {
	overrides := getToolOverrides(strings.TrimSpace(agentID))
	if len(overrides) == 0 {
		return tools
	}
	result := toolmanifest.Apply(tools, overrides)
	log.Infof("NPC 工具清单已应用: agent=%s 清单条目=%d 生效工具=%d", agentID, len(overrides), len(result))
	return result
}
