package npcagent

// scheduler.go — NPC 常驻 Agent 的定时决策回路
//
// 每个周期做三件事，构成「意图 → 行动 → 结果」：
//   1. 读状态：调用客户端的 npc_status，拿到这个 NPC 此刻在哪、在做什么；
//   2. 做决策：把状态与角色设定交给决策器（服务端 LLM），得到一个动作意图；
//   3. 下动作：把意图校验后经 npc_act 下发给客户端身体，拿到回执。
//
// 设计约束（对应服务器 406MB 内存红线）：
//   - 单 goroutine 顺序推进，不按 agent 起协程，常驻内存只有每 agent 一份小状态；
//   - 每个 agent 一个 nextDue 时间戳，扫描是 O(agent 数) 且无锁竞争；
//   - 决策超时必须明显小于最短决策周期，否则会挤占其它 NPC 的时间片。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrActionRejected 表示"动作被游戏端明确拒绝"（NPC 正在忙、已倒地、参数不合规等）。
// 这类结果不是基础设施故障：不该按指数退避惩罚，只记一次拒绝、下个周期再试。
var ErrActionRejected = errors.New("动作被拒绝")

// Bridge 与游戏客户端（每个 NPC 一条 MCP 通道）对话的抽象。
// 真实实现走 ws endpoint MCP 工具，测试里用假实现。
type Bridge interface {
	// Available 客户端是否在线；离线时本轮直接跳过，不计失败
	Available() bool
	// Status 读状态快照（npc_status 的返回文本）
	Status(ctx context.Context) (string, error)
	// Act 下发动作（npc_act 的工具入参，已扁平化）
	Act(ctx context.Context, args map[string]any) (string, error)
}

// Brain 决策器：把「我是谁 + 我现在什么状态」变成「我下一步做什么」
type Brain interface {
	Decide(ctx context.Context, req DecideRequest) (Decision, error)
}

// AgentSnapshot 供日志/排查用的只读快照
type AgentSnapshot struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IntervalSec int       `json:"interval_seconds"`
	Online      bool      `json:"online"`
	Runs        int       `json:"runs"`
	Skips       int       `json:"skips"`
	Failures    int       `json:"failures"`
	Rejects     int       `json:"rejects"`
	LastRunAt   time.Time `json:"last_run_at"`
	NextRunAt   time.Time `json:"next_run_at"`
	LastAction  string    `json:"last_action"`
	LastReason  string    `json:"last_reason"`
	LastReceipt string    `json:"last_receipt"`
	LastError   string    `json:"last_error"`
}

type agentState struct {
	cfg    AgentConfig
	bridge Bridge

	nextDue time.Time

	runs     int
	skips    int
	failures int
	rejects  int

	lastRunAt time.Time
	lastAct   string
	lastWhy   string
	lastReply string
	lastErr   string
}

// Scheduler 定时决策调度器
type Scheduler struct {
	cfg   Config
	brain Brain
	logf  func(format string, args ...any)
	now   func() time.Time

	mu     sync.Mutex
	states []*agentState
}

// Option 可选依赖注入（时钟/日志），便于测试确定性
type Option func(*Scheduler)

// WithClock 替换时钟（测试用）
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// WithLogger 替换日志输出
func WithLogger(logf func(format string, args ...any)) Option {
	return func(s *Scheduler) {
		if logf != nil {
			s.logf = logf
		}
	}
}

// New 构造调度器。bridges 按 agent id 提供通道，缺失的 agent 会被标记离线而不是报错。
func New(cfg Config, brain Brain, bridges map[string]Bridge, opts ...Option) (*Scheduler, error) {
	normalized, err := cfg.Normalize()
	if err != nil {
		return nil, err
	}
	if brain == nil {
		return nil, fmt.Errorf("npcagent: 决策器为空")
	}

	s := &Scheduler{
		cfg:   normalized,
		brain: brain,
		now:   time.Now,
		logf:  log.Printf,
	}
	for _, opt := range opts {
		opt(s)
	}

	base := s.now()
	for _, a := range normalized.Agents {
		if !a.IsEnabled() {
			continue
		}
		bridge := bridges[a.ID]
		s.states = append(s.states, &agentState{
			cfg:    a,
			bridge: bridge,
			// 首轮错开一个周期起跑，避免所有 agent 在同一秒抢同一个游戏连接
			nextDue: base.Add(a.Interval()),
		})
	}
	return s, nil
}

// Run 阻塞运行直到 ctx 取消。单 goroutine 顺序推进，不做并发调度。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.cfg.Enabled || len(s.states) == 0 {
		s.logf("[npcagent] 未启用或没有可运行的 agent，调度器不启动")
		return
	}
	interval := time.Duration(s.cfg.TickSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.logf("[npcagent] 调度器启动: agents=%d tick=%s", len(s.states), interval)

	for {
		select {
		case <-ctx.Done():
			s.logf("[npcagent] 调度器停止")
			return
		case <-ticker.C:
			s.Step(ctx)
		}
	}
}

// Step 推进一轮：只处理到期的 agent。测试直接调用它，不需要等 ticker。
func (s *Scheduler) Step(ctx context.Context) {
	now := s.now()

	s.mu.Lock()
	due := make([]*agentState, 0, len(s.states))
	for _, st := range s.states {
		if st.nextDue.After(now) {
			continue
		}
		// 先把下一次排上，避免单轮耗时过长时同一 agent 被连续触发
		st.nextDue = now.Add(st.cfg.Interval())
		due = append(due, st)
	}
	s.mu.Unlock()

	// 按已等待时长排序，久等的先跑，避免某个慢 LLM 长期饿死后面的 NPC
	sort.SliceStable(due, func(i, j int) bool { return due[i].lastRunAt.Before(due[j].lastRunAt) })

	for _, st := range due {
		if ctx.Err() != nil {
			return
		}
		s.runAgent(ctx, st)
	}
}

func (s *Scheduler) runAgent(ctx context.Context, st *agentState) {
	if st.bridge == nil || !st.bridge.Available() {
		s.mu.Lock()
		st.skips++
		s.mu.Unlock()
		s.logf("[npcagent] %s 离线，跳过本轮", st.cfg.ID)
		return
	}

	s.mu.Lock()
	st.runs++
	st.lastRunAt = s.now()
	s.mu.Unlock()

	statusCtx, cancelStatus := context.WithTimeout(ctx, time.Duration(s.cfg.StatusTimeoutSeconds)*time.Second)
	status, err := st.bridge.Status(statusCtx)
	cancelStatus()
	if err != nil {
		if errors.Is(err, ErrActionRejected) {
			// 读不到状态通常意味着这个 NPC 此刻不在场景里：本轮跳过，不算故障
			s.mu.Lock()
			st.skips++
			s.mu.Unlock()
			s.logf("[npcagent] %s 读不到状态，本轮跳过: %v", st.cfg.ID, err)
			return
		}
		s.fail(st, "读状态失败: "+err.Error())
		return
	}

	req := DecideRequest{
		AgentID: st.cfg.ID,
		Name:    st.cfg.Name,
		Persona: st.cfg.Persona,
		Status:  status,
		Allowed: st.cfg.AllowedActions,
	}
	decideCtx, cancelDecide := context.WithTimeout(ctx, time.Duration(s.cfg.DecideTimeoutSeconds)*time.Second)
	decision, err := s.brain.Decide(decideCtx, req)
	cancelDecide()
	if err != nil {
		s.fail(st, "决策失败: "+err.Error())
		return
	}

	if err := decision.Validate(st.cfg.AllowedActions, s.cfg.MaxReasonChars); err != nil {
		// 不合法的决策不是基础设施故障：不累计失败退避，只记录，下个周期重来
		s.mu.Lock()
		st.rejects++
		st.lastErr = err.Error()
		st.lastWhy = decision.Reason
		s.mu.Unlock()
		s.logf("[npcagent] %s 决策被拒: %v（原始动作=%q）", st.cfg.ID, err, decision.Action)
		return
	}

	actCtx, cancelAct := context.WithTimeout(ctx, time.Duration(s.cfg.ActTimeoutSeconds)*time.Second)
	receipt, err := st.bridge.Act(actCtx, decision.ActArgs())
	cancelAct()
	if err != nil {
		if errors.Is(err, ErrActionRejected) {
			// 游戏端说"现在做不了"：NPC 在忙/倒地/路不通。这不算故障，
			// 记一次拒绝（不推进退避），让下一个周期重新决策。
			s.mu.Lock()
			st.rejects++
			st.lastAct = decision.Action
			st.lastWhy = decision.Reason
			st.lastErr = err.Error()
			s.mu.Unlock()
			s.logf("[npcagent] %s 动作 %s 被拒绝: %v", st.cfg.ID, decision.Action, err)
			return
		}
		s.fail(st, "下发动作失败: "+err.Error())
		return
	}

	s.mu.Lock()
	st.failures = 0
	st.lastAct = decision.Action
	st.lastWhy = decision.Reason
	st.lastReply = truncateRunes(strings.TrimSpace(receipt), 300)
	st.lastErr = ""
	s.mu.Unlock()
	s.logf("[npcagent] %s 动作=%s 理由=%q 回执=%q", st.cfg.ID, decision.Action, decision.Reason, st.lastReply)
}

// fail 记录失败并按指数退避推迟下一次：服务端 LLM 抖动或游戏端离线时不至于把周期打满
func (s *Scheduler) fail(st *agentState, msg string) {
	s.mu.Lock()
	st.failures++
	st.lastErr = msg
	failures := st.failures

	backoff := time.Duration(s.cfg.FailureCooldownSeconds) * time.Second
	shift := failures - 1
	if shift > 4 {
		shift = 4
	}
	backoff <<= shift
	maxBackoff := time.Duration(s.cfg.MaxBackoffSeconds) * time.Second
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if backoff < st.cfg.Interval() {
		backoff = st.cfg.Interval()
	}
	st.nextDue = s.now().Add(backoff)
	s.mu.Unlock()

	s.logf("[npcagent] %s %s，%.0fs 后重试（连续失败 %d 次）", st.cfg.ID, msg, backoff.Seconds(), failures)
}

// Snapshot 返回各 agent 的只读状态，供日志/排查/后续管理接口使用
func (s *Scheduler) Snapshot() []AgentSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]AgentSnapshot, 0, len(s.states))
	for _, st := range s.states {
		online := st.bridge != nil && st.bridge.Available()
		out = append(out, AgentSnapshot{
			ID:          st.cfg.ID,
			Name:        st.cfg.Name,
			IntervalSec: int(st.cfg.Interval().Seconds()),
			Online:      online,
			Runs:        st.runs,
			Skips:       st.skips,
			Failures:    st.failures,
			Rejects:     st.rejects,
			LastRunAt:   st.lastRunAt,
			NextRunAt:   st.nextDue,
			LastAction:  st.lastAct,
			LastReason:  st.lastWhy,
			LastReceipt: st.lastReply,
			LastError:   st.lastErr,
		})
	}
	return out
}

// AgentCount 参与调度的 agent 数量
func (s *Scheduler) AgentCount() int {
	return len(s.states)
}
