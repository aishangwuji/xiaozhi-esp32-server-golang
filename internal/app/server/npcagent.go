package server

// npcagent.go — NPC 常驻 Agent 的服务端接线
//
// 把 domain 层的纯逻辑接到真实设施上：
//   - Bridge：游戏客户端（每个 NPC 一条 ws endpoint MCP 通道）暴露的 npc_status / npc_act
//   - Brain  ：走 llm.LLMProvider，用 npcagent.BuildPrompt 生成的提示词要一个 JSON 决策
//
// 为什么桥接层不放进 domain/npcagent：那两个包依赖 cgo 音频与 eino，会让纯逻辑包无法单测。
// 这里只做装配，不含任何决策逻辑。
//
// 红线（对齐项目 AGENTS.md）：
//   - 不在这里判断玩家意图，也不做关键词兜底：决策完全由模型给出，本层只校验契约；
//   - 游戏端离线/超时属基础设施异常，跳过或退避，不伪造动作。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
	"github.com/spf13/viper"

	"xiaozhi-esp32-server-golang/internal/domain/llm"
	"xiaozhi-esp32-server-golang/internal/domain/mcp"
	"xiaozhi-esp32-server-golang/internal/domain/npcagent"
	log "xiaozhi-esp32-server-golang/logger"
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
			return npcagent.Decision{}, errors.New(llm.LLMErrorMessage(msg))
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
		return npcagent.Decision{}, errors.New("模型没有返回内容")
	}
	return npcagent.ParseDecision(sb.String())
}

// loadNpcAgentConfig 从 main_config.yaml 的 npc_agent 段读配置
func loadNpcAgentConfig() (npcagent.Config, error) {
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

// buildNpcAgentLLM 创建决策用的 LLM 提供者。
// 独立于设备会话的 LLM 配置：NPC 的日常决策是服务端自己的开销，不该混用某个设备的模型额度。
func buildNpcAgentLLM() (llm.LLMProvider, error) {
	providerName := strings.TrimSpace(viper.GetString("npc_agent.llm.provider"))
	if providerName == "" {
		return nil, errors.New("未配置 npc_agent.llm.provider")
	}
	providerConfig := viper.GetStringMap("npc_agent.llm." + providerName)
	if len(providerConfig) == 0 {
		return nil, fmt.Errorf("未找到 npc_agent.llm.%s 配置块", providerName)
	}
	return llm.GetLLMProvider(providerName, providerConfig)
}

// StartNpcAgentScheduler 按配置装配并启动调度器；未启用时返回 nil 且不做任何事。
// 失败只记日志不 panic：NPC 不自己走动不应该拖垮整个语音服务。
func StartNpcAgentScheduler(ctx context.Context) {
	cfg, err := loadNpcAgentConfig()
	if err != nil {
		log.Errorf("[npcagent] %v", err)
		return
	}
	normalized, err := cfg.Normalize()
	if err != nil {
		log.Errorf("[npcagent] 配置无效: %v", err)
		return
	}
	if !normalized.Enabled || len(normalized.Agents) == 0 {
		log.Infof("[npcagent] 未启用（npc_agent.enabled），NPC 只会在收到工具调用时行动")
		return
	}

	provider, err := buildNpcAgentLLM()
	if err != nil {
		log.Errorf("[npcagent] 初始化决策模型失败: %v", err)
		return
	}
	brain := &npcLLMBrain{provider: provider, maxStatusChars: normalized.MaxStatusChars}

	bridges := make(map[string]npcagent.Bridge, len(normalized.Agents))
	for _, agent := range normalized.Agents {
		bridges[agent.ID] = newNpcMcpBridge(agent.ID)
	}

	scheduler, err := npcagent.New(normalized, brain, bridges)
	if err != nil {
		log.Errorf("[npcagent] 构造调度器失败: %v", err)
		return
	}

	go scheduler.Run(ctx)
	log.Infof("[npcagent] 已启动: agents=%d", scheduler.AgentCount())
	for _, snap := range scheduler.Snapshot() {
		log.Infof("[npcagent] agent=%s name=%s interval=%ds online=%v",
			snap.ID, snap.Name, snap.IntervalSec, snap.Online)
	}
}
