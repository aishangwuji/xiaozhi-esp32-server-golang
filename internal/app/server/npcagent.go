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

	"github.com/cloudwego/eino/components/tool"
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
	return connected
}

func (b *npcMcpBridge) runTool(ctx context.Context, name string, argsJSON string) (string, error) {
	tools, err := mcp.GetWsEndpointMcpTools(b.agentID)
	if err != nil {
		return "", fmt.Errorf("读取 %s 的工具清单失败: %w", b.agentID, err)
	}
	invokable, ok := tools[name]
	if !ok {
		return "", fmt.Errorf("%s 上没有工具 %s（游戏端可能未连接）", b.agentID, name)
	}
	return invokeTool(ctx, invokable, name, argsJSON)
}

func (b *npcMcpBridge) Status(ctx context.Context) (string, error) {
	return b.runTool(ctx, "npc_status", "{}")
}

func (b *npcMcpBridge) Act(ctx context.Context, args map[string]any) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("动作入参序列化失败: %w", err)
	}
	return b.runTool(ctx, "npc_act", string(payload))
}

// invokeTool 调用设备侧 MCP 工具；工具侧返回的错误文本要当失败抛出，
// 否则调度器会把"动作没做成"记成成功，结果就是 NPC 永远站在原地还以为在干活。
func invokeTool(ctx context.Context, invokable tool.InvokableTool, name string, argsJSON string) (string, error) {
	out, err := invokable.InvokableRun(ctx, argsJSON)
	if err != nil {
		return "", fmt.Errorf("调用工具 %s 失败: %w", name, err)
	}
	return out, nil
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
