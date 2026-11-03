package npcagent

// config.go — NPC 常驻 Agent 的配置结构与默认值
//
// 配置来源：main_config.yaml 的 npc_agent 段（由 internal/app/server 侧用 viper 反序列化后传入）。
// 本包不直接读配置也不读环境变量，保持"纯逻辑"，便于单测。
//
// 每个 agent 对应游戏里一个 NPC（同一 agent_id 就是游戏 MCP 通道的 JWT 主体）。
// 缺省关闭：没有显式 enabled: true 时不会启动任何定时决策。

import (
	"fmt"
	"strings"
	"time"
)

// 决策动作白名单：必须与客户端 npc_act 的 action 枚举一致。
// 这里只做契约校验，不参与"该做什么"的判断。
var DefaultAllowedActions = []string{"walk", "run", "jump", "talk", "attack", "wait", "stop"}

const (
	defaultTickSeconds        = 1
	defaultIntervalSeconds    = 30
	defaultFailureCooldownSec = 60
	defaultMaxBackoffSeconds  = 600
	defaultStatusTimeoutSec   = 8
	defaultActTimeoutSec      = 8
	defaultDecideTimeoutSec   = 25
	defaultMaxStatusChars     = 1200
	defaultMaxReasonChars     = 120
)

// AgentConfig 单个 NPC Agent 的配置
type AgentConfig struct {
	// ID 必须与游戏侧 MCP 通道所属 agent 一致（JWT 的 agentId）
	ID string `mapstructure:"id" json:"id"`
	// Name 仅用于日志与提示词
	Name string `mapstructure:"name" json:"name"`
	// Persona 角色设定（人设/职责/说话风格），拼进决策提示词
	Persona string `mapstructure:"persona" json:"persona"`
	// IntervalSeconds 决策周期；0 表示用 Config.DefaultIntervalSeconds
	IntervalSeconds int `mapstructure:"interval_seconds" json:"interval_seconds"`
	// Enabled 显式关闭某个 agent；nil 视为启用
	Enabled *bool `mapstructure:"enabled" json:"enabled"`
	// AllowedActions 该 NPC 允许的动作子集；空表示用 DefaultAllowedActions
	AllowedActions []string `mapstructure:"allowed_actions" json:"allowed_actions"`
}

// Config 调度器配置
type Config struct {
	Enabled                bool          `mapstructure:"enabled" json:"enabled"`
	TickSeconds            int           `mapstructure:"tick_seconds" json:"tick_seconds"`
	DefaultIntervalSeconds int           `mapstructure:"default_interval_seconds" json:"default_interval_seconds"`
	FailureCooldownSeconds int           `mapstructure:"failure_cooldown_seconds" json:"failure_cooldown_seconds"`
	MaxBackoffSeconds      int           `mapstructure:"max_backoff_seconds" json:"max_backoff_seconds"`
	StatusTimeoutSeconds   int           `mapstructure:"status_timeout_seconds" json:"status_timeout_seconds"`
	ActTimeoutSeconds      int           `mapstructure:"act_timeout_seconds" json:"act_timeout_seconds"`
	DecideTimeoutSeconds   int           `mapstructure:"decide_timeout_seconds" json:"decide_timeout_seconds"`
	MaxStatusChars         int           `mapstructure:"max_status_chars" json:"max_status_chars"`
	MaxReasonChars         int           `mapstructure:"max_reason_chars" json:"max_reason_chars"`
	Agents                 []AgentConfig `mapstructure:"agents" json:"agents"`
}

// Normalize 填充默认值、剔除无效 agent，并返回可用于运行时的规范配置。
// 无效 agent（空 id / 重复 id）会被剔除而不是让整个调度器失败。
func (c Config) Normalize() (Config, error) {
	out := c
	if out.TickSeconds <= 0 {
		out.TickSeconds = defaultTickSeconds
	}
	if out.DefaultIntervalSeconds <= 0 {
		out.DefaultIntervalSeconds = defaultIntervalSeconds
	}
	if out.FailureCooldownSeconds <= 0 {
		out.FailureCooldownSeconds = defaultFailureCooldownSec
	}
	if out.MaxBackoffSeconds <= 0 {
		out.MaxBackoffSeconds = defaultMaxBackoffSeconds
	}
	if out.MaxBackoffSeconds < out.FailureCooldownSeconds {
		out.MaxBackoffSeconds = out.FailureCooldownSeconds
	}
	if out.StatusTimeoutSeconds <= 0 {
		out.StatusTimeoutSeconds = defaultStatusTimeoutSec
	}
	if out.ActTimeoutSeconds <= 0 {
		out.ActTimeoutSeconds = defaultActTimeoutSec
	}
	if out.DecideTimeoutSeconds <= 0 {
		out.DecideTimeoutSeconds = defaultDecideTimeoutSec
	}
	if out.MaxStatusChars <= 0 {
		out.MaxStatusChars = defaultMaxStatusChars
	}
	if out.MaxReasonChars <= 0 {
		out.MaxReasonChars = defaultMaxReasonChars
	}

	seen := make(map[string]struct{}, len(out.Agents))
	agents := make([]AgentConfig, 0, len(out.Agents))
	for _, a := range out.Agents {
		a.ID = strings.TrimSpace(a.ID)
		if a.ID == "" {
			continue
		}
		if _, dup := seen[a.ID]; dup {
			continue
		}
		seen[a.ID] = struct{}{}
		a.Name = strings.TrimSpace(a.Name)
		a.Persona = strings.TrimSpace(a.Persona)
		if a.IntervalSeconds <= 0 {
			a.IntervalSeconds = out.DefaultIntervalSeconds
		}
		if len(a.AllowedActions) == 0 {
			a.AllowedActions = append([]string(nil), DefaultAllowedActions...)
		}
		agents = append(agents, a)
	}
	out.Agents = agents

	if out.Enabled && len(agents) == 0 {
		return out, fmt.Errorf("npc_agent.enabled=true 但没有配置任何 agent")
	}
	return out, nil
}

// Interval 该 agent 的决策周期
func (a AgentConfig) Interval() time.Duration {
	if a.IntervalSeconds <= 0 {
		return defaultIntervalSeconds * time.Second
	}
	return time.Duration(a.IntervalSeconds) * time.Second
}

// IsEnabled 该 agent 是否启用（未显式设置视为启用）
func (a AgentConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}
