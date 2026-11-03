package server

// npcagent.go — NPC 常驻 Agent 的服务端接线
//
// 把 domain 层的纯逻辑接到真实设施上：
//   - Bridge：游戏客户端（每个 NPC 一条 ws endpoint MCP 通道）暴露的 npc_status / npc_act
//   - Brain  ：走 llm.LLMProvider，用 npcagent.BuildPrompt 生成的提示词要一个 JSON 决策
//   - 配置   ：main_config.yaml 的 npc_agent 段做引导；控制台库（/api/internal/npc-agents）
//     接管后每 60s 拉一次并热更新，改完不用重启
//   - 状态   ：NpcAgentStatusSnapshot() 供 GET /internal/npc-agents/status 只读暴露
//
// 为什么桥接层不放进 domain/npcagent：那个包要求零 cgo 依赖以便单测；这里只做装配，
// 不含任何决策逻辑。
//
// 红线（对齐项目 AGENTS.md）：
//   - 不在这里判断玩家意图，也不做关键词兜底：决策完全由模型给出，本层只校验契约；
//   - 游戏端离线/超时属基础设施异常，跳过或退避，不伪造动作。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/spf13/viper"

	componentshttp "xiaozhi-esp32-server-golang/internal/components/http"
	"xiaozhi-esp32-server-golang/internal/domain/llm"
	"xiaozhi-esp32-server-golang/internal/domain/mcp"
	"xiaozhi-esp32-server-golang/internal/domain/npcagent"
	"xiaozhi-esp32-server-golang/internal/util"
	log "xiaozhi-esp32-server-golang/logger"
)

const (
	// npcAgentConfigRefresh 配置热更新周期：控制台改完最迟这么久生效
	npcAgentConfigRefresh = 60 * time.Second
	// npcAgentFetchTimeout 拉配置的超时：走本机回环，正常是毫秒级
	npcAgentFetchTimeout = 5 * time.Second
)

// npcMcpBridge 通过 ws endpoint MCP 通道操作某个 NPC 的身体
type npcMcpBridge struct {
	agentID string
	// 离线提示只打一次：每个周期刷一行"离线"会把日志淹掉，反而看不到真正的错误
	offlineOnce sync.Once
}

func newNpcMcpBridge(agentID string) *npcMcpBridge {
	return &npcMcpBridge{agentID: strings.TrimSpace(agentID)}
}

// Available 游戏端是否在线。未连上时整轮跳过，不算失败，避免刚启动就连环退避。
func (b *npcMcpBridge) Available() bool {
	if b.agentID == "" {
		return false
	}
	connected, _ := mcp.GetWsEndpointConnectionStatus(b.agentID)
	if connected {
		return true
	}
	b.offlineOnce.Do(func() {
		// agentId 藏在游戏侧 token 里，写错不会报错、只会永远"离线"。
		// 这里把当前真正在线的通道打出来，让配置错误一眼可见。
		if others := mcp.ListConnectedWsEndpointAgentIDs(); len(others) > 0 {
			log.Warnf("[npcagent] 配置的 agent=%s 没有在线通道，但当前在线的是 %v —— 检查 agentId 是否与游戏侧 JWT 一致",
				b.agentID, others)
		} else {
			log.Infof("[npcagent] agent=%s 暂无在线通道，等游戏端连上来再开始驱动", b.agentID)
		}
	})
	return false
}

// runTool 调用设备侧工具，返回文本 + 是否被业务拒绝（rejected）+ 调用错误。
// 三者必须分开：rejected 是"NPC 现在做不了"，调用错误才是"通道坏了"。
func (b *npcMcpBridge) runTool(ctx context.Context, name string, argsJSON string) (string, bool, error) {
	tools, err := mcp.GetWsEndpointMcpTools(b.agentID)
	if err != nil {
		return "", false, fmt.Errorf("读取 %s 的工具清单失败: %w", b.agentID, err)
	}
	invokable, ok := tools[name]
	if !ok {
		// 客户端版本不支持该工具：属于能力不可用，不是通道故障
		return "", true, fmt.Errorf("%s 上没有工具 %s（游戏端可能未连接或版本过旧）", b.agentID, name)
	}
	out, err := invokable.InvokableRun(ctx, argsJSON)
	if err != nil {
		return "", false, fmt.Errorf("调用工具 %s 失败: %w", name, err)
	}
	text, isErr := parseToolResult(out)
	return text, isErr, nil
}

func (b *npcMcpBridge) Status(ctx context.Context) (string, error) {
	text, rejected, err := b.runTool(ctx, "npc_status", "{}")
	if err != nil {
		return "", err
	}
	if rejected {
		return text, fmt.Errorf("%w: %s", npcagent.ErrActionRejected, text)
	}
	return text, nil
}

func (b *npcMcpBridge) Act(ctx context.Context, args map[string]any) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("动作入参序列化失败: %w", err)
	}
	text, rejected, err := b.runTool(ctx, "npc_act", string(payload))
	if err != nil {
		return text, err
	}
	if rejected {
		// NPC 正在忙/倒地/走不通：本轮没做成，但通道是好的，不该退避
		return text, fmt.Errorf("%w: %s", npcagent.ErrActionRejected, text)
	}
	return text, nil
}

// mcpToolResult 是 MCP tools/call 的标准返回结构。
// 游戏侧的 isError 是业务失败（例如"动作参数不合法""NPC 已力竭倒地"），
// 必须当错误处理，否则调度器会把"没做成"记成成功、下次还发同样的动作。
type mcpToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// parseToolResult 从 MCP 返回里取出文本与错误标记；不是标准结构时整段当文本。
func parseToolResult(out string) (string, bool) {
	var result mcpToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return out, false
	}
	parts := make([]string, 0, len(result.Content))
	for _, item := range result.Content {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		text = out
	}
	return text, result.IsError
}

// npcLLMBrain 用服务端配置的 LLM 做一次决策
type npcLLMBrain struct {
	provider llm.LLMProvider
	// maxStatusChars 状态文本截断长度：游戏端快照较长，全量塞进上下文既浪费 token 也拖慢决策
	maxStatusChars int
}

func (b *npcLLMBrain) Decide(ctx context.Context, req npcagent.DecideRequest) (npcagent.Decision, error) {
	system, user := npcagent.BuildPrompt(req, b.maxStatusChars)
	messages := []*schema.Message{
		schema.SystemMessage(system),
		schema.UserMessage(user),
	}

	// 决策不需要工具调用：动作契约由 JSON 表达，避免多轮工具流带来的不确定性与额外 token
	stream := b.provider.ResponseWithContext(ctx, "npcagent:"+req.AgentID, messages, nil)

	var sb strings.Builder
	truncated := false
	for msg := range stream {
		if msg == nil {
			continue
		}
		if llm.IsLLMErrorMessage(msg) {
			return npcagent.Decision{}, fmt.Errorf("%s", llm.LLMErrorMessage(msg))
		}
		if truncated {
			continue // 继续排空 channel，避免上游 goroutine 卡在写入上
		}
		sb.WriteString(msg.Content)
		// 决策输出应该很短；异常长的输出直接丢弃后续内容，防止异常模型刷满上下文
		if sb.Len() > 8192 {
			truncated = true
		}
	}
	if strings.TrimSpace(sb.String()) == "" {
		return npcagent.Decision{}, fmt.Errorf("模型没有返回内容")
	}
	return npcagent.ParseDecision(sb.String())
}

// ── 运行期状态 ──

// npcAgentRuntime 常驻单例：调度器 + 当前生效配置 + 装配错误
type npcAgentRuntime struct {
	mu sync.RWMutex
	// sched 首次启用时才创建：未启用时没必要校验模型密钥
	sched *npcagent.Scheduler
	cfg   npcagent.Config
	// errMsg 装配失败原因（缺 provider/密钥等），原样透给控制台，
	// 避免"页面显示已开启但 NPC 不动"这种无解排查
	errMsg string
	// configSource 当前配置来源：yaml / manager
	configSource string
	appliedAt    time.Time
}

var npcAgentRT = &npcAgentRuntime{}

// NpcAgentStatusSnapshot 只读状态快照（无任何副作用），供 HTTP 接口与控制台使用
func NpcAgentStatusSnapshot() map[string]any {
	npcAgentRT.mu.RLock()
	cfg := npcAgentRT.cfg
	sched := npcAgentRT.sched
	errMsg := npcAgentRT.errMsg
	source := npcAgentRT.configSource
	appliedAt := npcAgentRT.appliedAt
	npcAgentRT.mu.RUnlock()

	agents := []npcagent.AgentSnapshot{}
	running := 0
	if sched != nil {
		agents = sched.Snapshot()
		running = sched.AgentCount()
	}

	provider := strings.TrimSpace(viper.GetString("npc_agent.llm.provider"))
	modelName := strings.TrimSpace(viper.GetString("npc_agent.llm." + provider + ".model_name"))

	return map[string]any{
		"enabled":           cfg.Enabled,
		"running":           sched != nil,
		"agent_count":       running,
		"tick_seconds":      cfg.TickSeconds,
		"default_interval":  cfg.DefaultIntervalSeconds,
		"config_source":     source,
		"config_applied_at": appliedAt,
		"error":             errMsg,
		// 决策模型只报告 provider/model，不回显任何密钥
		"llm": map[string]any{
			"provider":   provider,
			"model_name": modelName,
		},
		"agents": agents,
	}
}

// ── 配置：yaml 引导 + 控制台接管 ──

// managerNpcAgentsPayload 控制台内部接口的响应（字段与 controllers/npc_agent.go 对齐）
type managerNpcAgentsPayload struct {
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
		ID              string   `json:"id"`
		Name            string   `json:"name"`
		Persona         string   `json:"persona"`
		IntervalSeconds int      `json:"interval_seconds"`
		Enabled         bool     `json:"enabled"`
		AllowedActions  []string `json:"allowed_actions"`
	} `json:"agents"`
}

var npcAgentManagerHTTPOnce sync.Once
var npcAgentManagerHTTP *componentshttp.ManagerClient

func getNpcAgentManagerClient() *componentshttp.ManagerClient {
	npcAgentManagerHTTPOnce.Do(func() {
		npcAgentManagerHTTP = componentshttp.NewManagerClient(componentshttp.ManagerClientConfig{
			BaseURL:    util.GetBackendURL(),
			AuthToken:  util.GetManagerAuthToken(),
			Timeout:    npcAgentFetchTimeout,
			MaxRetries: 1,
		})
	})
	return npcAgentManagerHTTP
}

// loadYamlConfig 读 main_config.yaml 的 npc_agent 段
func loadYamlConfig() (npcagent.Config, error) {
	var cfg npcagent.Config
	sub := viper.Sub("npc_agent")
	if sub == nil {
		return cfg, nil
	}
	if err := sub.Unmarshal(&cfg); err != nil {
		return cfg, fmt.Errorf("解析 npc_agent 配置失败: %w", err)
	}
	return cfg, nil
}

// loadYamlConfigSafe 热更新周期里读 yaml：读失败时沿用当前生效配置。
// 绝不能回退成空配置——那等于把 Enabled 静默改成 false，NPC 会毫无解释地停下。
func loadYamlConfigSafe() npcagent.Config {
	cfg, err := loadYamlConfig()
	if err != nil {
		log.Warnf("[npcagent] 重新读取 yaml 配置失败（沿用当前配置）: %v", err)
		npcAgentRT.mu.RLock()
		current := npcAgentRT.cfg
		npcAgentRT.mu.RUnlock()
		return current
	}
	return cfg
}

// fetchManagerConfig 拉控制台配置；返回 nil 表示控制台还没接管或不可达
func fetchManagerConfig(ctx context.Context) *managerNpcAgentsPayload {
	raw, err := getNpcAgentManagerClient().DoRequestRaw(ctx, componentshttp.RequestOptions{
		Method: "GET",
		Path:   "/api/internal/npc-agents",
	})
	if err != nil {
		log.Warnf("[npcagent] 拉取控制台配置失败（沿用当前配置）: %v", err)
		return nil
	}
	var payload managerNpcAgentsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Warnf("[npcagent] 控制台配置解析失败（沿用当前配置）: %v", err)
		return nil
	}
	return &payload
}

// resolveConfig 合并 yaml 与控制台配置。规则（控制台页面文案与此一致）：
//   - settings_managed=true → 控制台的全局设置接管 yaml 同类字段
//   - agents_managed=true   → 控制台的 agent 列表接管 yaml 的 agents（删掉即停）
//   - 都不满足               → 纯 yaml 生效（没登录过控制台也能跑）
func resolveConfig(yamlCfg npcagent.Config, remote *managerNpcAgentsPayload) (npcagent.Config, string) {
	if remote == nil {
		return yamlCfg, "yaml"
	}
	merged := yamlCfg
	source := "yaml"

	if remote.SettingsManaged {
		merged.Enabled = remote.Settings.Enabled
		if remote.Settings.TickSeconds > 0 {
			merged.TickSeconds = remote.Settings.TickSeconds
		}
		if remote.Settings.DefaultIntervalSeconds > 0 {
			merged.DefaultIntervalSeconds = remote.Settings.DefaultIntervalSeconds
		}
		if remote.Settings.DecideTimeoutSeconds > 0 {
			merged.DecideTimeoutSeconds = remote.Settings.DecideTimeoutSeconds
		}
		if remote.Settings.MaxStatusChars > 0 {
			merged.MaxStatusChars = remote.Settings.MaxStatusChars
		}
		source = "manager"
	}

	if remote.AgentsManaged {
		agents := make([]npcagent.AgentConfig, 0, len(remote.Agents))
		for _, a := range remote.Agents {
			enabled := a.Enabled
			agents = append(agents, npcagent.AgentConfig{
				ID:              strings.TrimSpace(a.ID),
				Name:            a.Name,
				Persona:         a.Persona,
				IntervalSeconds: a.IntervalSeconds,
				Enabled:         &enabled,
				AllowedActions:  a.AllowedActions,
			})
		}
		merged.Agents = agents
		source = "manager"
	}
	return merged, source
}

// buildNpcAgentLLM 创建决策用的 LLM 提供者。
// 独立于设备会话的 LLM 配置：NPC 的日常决策是服务端自己的开销，不该混用某个设备的模型额度。
func buildNpcAgentLLM() (llm.LLMProvider, string, error) {
	providerName := strings.TrimSpace(viper.GetString("npc_agent.llm.provider"))
	if providerName == "" {
		return nil, "", fmt.Errorf("未配置 npc_agent.llm.provider（决策模型），NPC 无法自主决策")
	}
	providerConfig := viper.GetStringMap("npc_agent.llm." + providerName)
	if len(providerConfig) == 0 {
		return nil, providerName, fmt.Errorf("未找到 npc_agent.llm.%s 配置块", providerName)
	}
	provider, err := llm.GetLLMProvider(providerName, providerConfig)
	if err != nil {
		return nil, providerName, err
	}
	return provider, providerName, nil
}

// applyConfig 应用一份配置：首次启用时创建调度器，之后只做热更新
func (rt *npcAgentRuntime) applyConfig(ctx context.Context, cfg npcagent.Config, source string) {
	normalized, err := cfg.Normalize()
	if err != nil {
		rt.mu.Lock()
		rt.errMsg = err.Error()
		rt.mu.Unlock()
		log.Errorf("[npcagent] 配置无效: %v", err)
		return
	}

	rt.mu.RLock()
	existing := rt.sched
	rt.mu.RUnlock()

	if existing == nil {
		if !normalized.Enabled {
			rt.mu.Lock()
			rt.cfg = normalized
			rt.configSource = source
			rt.appliedAt = time.Now()
			rt.errMsg = ""
			rt.mu.Unlock()
			log.Infof("[npcagent] 未启用：NPC 只在收到工具调用时行动（可在控制台开启）")
			return
		}
		provider, providerName, err := buildNpcAgentLLM()
		if err != nil {
			rt.mu.Lock()
			rt.cfg = normalized
			rt.configSource = source
			rt.appliedAt = time.Now()
			rt.errMsg = err.Error()
			rt.mu.Unlock()
			log.Errorf("[npcagent] 决策模型初始化失败: %v", err)
			return
		}
		brain := &npcLLMBrain{provider: provider, maxStatusChars: normalized.MaxStatusChars}
		sched, err := npcagent.New(normalized, brain, nil,
			npcagent.WithBridgeResolver(func(agentID string) npcagent.Bridge {
				return newNpcMcpBridge(agentID)
			}))
		if err != nil {
			rt.mu.Lock()
			rt.cfg = normalized
			rt.configSource = source
			rt.appliedAt = time.Now()
			rt.errMsg = err.Error()
			rt.mu.Unlock()
			log.Errorf("[npcagent] 构造调度器失败: %v", err)
			return
		}
		go sched.Run(ctx)

		rt.mu.Lock()
		rt.sched = sched
		rt.cfg = normalized
		rt.configSource = source
		rt.appliedAt = time.Now()
		rt.errMsg = ""
		rt.mu.Unlock()

		log.Infof("[npcagent] 已启动: agents=%d provider=%s source=%s", sched.AgentCount(), providerName, source)
		return
	}

	if err := existing.Reload(normalized); err != nil {
		rt.mu.Lock()
		rt.errMsg = err.Error()
		rt.mu.Unlock()
		log.Errorf("[npcagent] 热更新失败（保持原计划）: %v", err)
		return
	}

	rt.mu.Lock()
	rt.cfg = existing.EffectiveConfig()
	rt.configSource = source
	rt.appliedAt = time.Now()
	rt.errMsg = ""
	rt.mu.Unlock()
	log.Infof("[npcagent] 配置已热更新: agents=%d enabled=%v source=%s",
		existing.AgentCount(), existing.Enabled(), source)
}

// StartNpcAgentScheduler 装配并启动调度器，随后每 60s 拉一次控制台配置做热更新。
// 失败只记日志不 panic：NPC 不自己走动不应该拖垮整个语音服务。
func StartNpcAgentScheduler(ctx context.Context) {
	yamlCfg, err := loadYamlConfig()
	if err != nil {
		log.Errorf("[npcagent] %v", err)
		return
	}

	// 先按 yaml 起，再立刻拉一次控制台配置覆盖，避免启动后 60s 内跑旧配置
	cfg, source := resolveConfig(yamlCfg, fetchManagerConfig(ctx))
	npcAgentRT.applyConfig(ctx, cfg, source)

	go func() {
		ticker := time.NewTicker(npcAgentConfigRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				next, nextSource := resolveConfig(loadYamlConfigSafe(), fetchManagerConfig(ctx))
				npcAgentRT.applyConfig(ctx, next, nextSource)
			}
		}
	}()
}
