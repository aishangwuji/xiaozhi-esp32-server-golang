package npcagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── 测试替身 ──

type fakeBridge struct {
	online   bool
	status   string
	statusEr error
	actEr    error

	mu       sync.Mutex
	statusIn int
	actArgs  []map[string]any
}

func (f *fakeBridge) Available() bool { return f.online }

func (f *fakeBridge) Status(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusIn++
	return f.status, f.statusEr
}

func (f *fakeBridge) Act(ctx context.Context, args map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actArgs = append(f.actArgs, args)
	if f.actEr != nil {
		return "", f.actEr
	}
	return "（已开始执行）", nil
}

type fakeBrain struct {
	decision Decision
	err      error
	raw      string

	mu       sync.Mutex
	requests []DecideRequest
}

func (f *fakeBrain) Decide(ctx context.Context, req DecideRequest) (Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if f.err != nil {
		return Decision{}, f.err
	}
	return f.decision, nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func quietLogger(string, ...any) {}

func testConfig(intervalSec int) Config {
	enabled := true
	return Config{
		Enabled:                true,
		TickSeconds:            1,
		DefaultIntervalSeconds: intervalSec,
		Agents: []AgentConfig{{
			ID:      "bob",
			Name:    "铁匠鲍勃",
			Persona: "固执的铁匠",
			Enabled: &enabled,
		}},
	}
}

// ── 配置 ──

func TestConfigNormalize(t *testing.T) {
	disabled := false
	cfg := Config{
		Agents: []AgentConfig{
			{ID: " bob "},
			{ID: "bob"}, // 重复，丢弃
			{ID: ""},    // 空 id，丢弃
			{ID: "dave", Enabled: &disabled},
		},
	}
	out, err := cfg.Normalize()
	if err != nil {
		t.Fatalf("Normalize 不应报错: %v", err)
	}
	if len(out.Agents) != 2 {
		t.Fatalf("应保留 2 个 agent，实际 %d", len(out.Agents))
	}
	if out.Agents[0].ID != "bob" {
		t.Fatalf("id 应被裁剪空白，实际 %q", out.Agents[0].ID)
	}
	if out.Agents[0].IntervalSeconds != defaultIntervalSeconds {
		t.Fatalf("应填入默认周期，实际 %d", out.Agents[0].IntervalSeconds)
	}
	if len(out.Agents[0].AllowedActions) != len(DefaultAllowedActions) {
		t.Fatalf("应填入默认动作白名单，实际 %v", out.Agents[0].AllowedActions)
	}
	if out.Agents[1].IsEnabled() {
		t.Fatalf("显式 enabled=false 应生效")
	}
	if out.TickSeconds != 1 || out.MaxBackoffSeconds < out.FailureCooldownSeconds {
		t.Fatalf("默认值不完整: %+v", out)
	}
}

func TestConfigEnabledWithoutAgents(t *testing.T) {
	_, err := Config{Enabled: true}.Normalize()
	if err == nil {
		t.Fatal("enabled=true 且没有 agent 时应报错，否则会静默不做事")
	}
}

// ── 决策解析与校验 ──

func TestParseDecisionForms(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantAct string
		wantErr bool
	}{
		{"纯 JSON", `{"action":"wait","params":{"duration":5},"reason":"站着"}`, "wait", false},
		{"代码块包裹", "```json\n{\"action\":\"walk\",\"params\":{\"x\":1,\"z\":2}}\n```", "walk", false},
		{"前后有废话", `好的，我的决定是：{"action":"jump"} 就这样。`, "jump", false},
		{"大写字动归一化", `{"action":"WAIT"}`, "wait", false},
		{"空输出", "   ", "", true},
		{"没有 JSON", "我不太确定该做什么", "", true},
		{"坏 JSON", `{"action":}`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := ParseDecision(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if d.Action != c.wantAct {
				t.Fatalf("动作应为 %q，实际 %q", c.wantAct, d.Action)
			}
		})
	}
}

func TestParseDecisionKeepsIntegerCoords(t *testing.T) {
	d, err := ParseDecision(`{"action":"walk","params":{"x":12,"z":-3.5}}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 校验阶段会把 json.Number 收敛成 int64/float64，坐标不能变成字符串
	if err := d.Validate(DefaultAllowedActions, 120); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if v, ok := d.Params["x"].(int64); !ok || v != 12 {
		t.Fatalf("x 应保留整数，实际 %#v", d.Params["x"])
	}
	if v, ok := d.Params["z"].(float64); !ok || v != -3.5 {
		t.Fatalf("z 应为浮点，实际 %#v", d.Params["z"])
	}
}

func TestValidateDecision(t *testing.T) {
	allowed := DefaultAllowedActions

	t.Run("动作不在白名单", func(t *testing.T) {
		d := Decision{Action: "fly"}
		if err := d.Validate(allowed, 120); err == nil {
			t.Fatal("应拒绝未登记动作")
		}
	})

	t.Run("未知参数被丢弃", func(t *testing.T) {
		d := Decision{Action: "wait", Params: map[string]any{"duration": 3, "teleport": "true"}}
		if err := d.Validate(allowed, 120); err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if _, ok := d.Params["teleport"]; ok {
			t.Fatal("未登记参数必须被丢掉，不能透传到游戏客户端")
		}
	})

	t.Run("嵌套参数被丢弃", func(t *testing.T) {
		d := Decision{Action: "wait", Params: map[string]any{"duration": map[string]any{"a": 1}}}
		if err := d.Validate(allowed, 120); err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if _, ok := d.Params["duration"]; ok {
			t.Fatal("嵌套对象不是合法参数，应丢弃")
		}
	})

	t.Run("walk 缺目标", func(t *testing.T) {
		d := Decision{Action: "walk", Params: map[string]any{"hold_seconds": 3}}
		if err := d.Validate(allowed, 120); err == nil {
			t.Fatal("walk 没有 location 也没有 x/z 时应拒绝")
		}
	})

	t.Run("walk 只给 x", func(t *testing.T) {
		d := Decision{Action: "walk", Params: map[string]any{"x": 3}}
		if err := d.Validate(allowed, 120); err == nil {
			t.Fatal("只给 x 应拒绝，避免 z 被当成 0 乱走")
		}
	})

	t.Run("talk 缺 target 默认 player", func(t *testing.T) {
		d := Decision{Action: "talk", Params: map[string]any{"say": "你好"}}
		if err := d.Validate(allowed, 120); err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if d.Params["target"] != "player" {
			t.Fatalf("target 应默认 player，实际 %v", d.Params["target"])
		}
	})

	t.Run("理由被截断", func(t *testing.T) {
		d := Decision{Action: "wait", Reason: strings.Repeat("长", 300)}
		if err := d.Validate(allowed, 20); err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if len([]rune(d.Reason)) > 21 {
			t.Fatalf("理由应被截断到 20 字左右，实际 %d", len([]rune(d.Reason)))
		}
	})
}

func TestBuildPrompt(t *testing.T) {
	system, user := BuildPrompt(DecideRequest{
		AgentID: "bob",
		Name:    "铁匠鲍勃",
		Persona: "固执的铁匠",
		Status:  strings.Repeat("状", 500),
		Allowed: DefaultAllowedActions,
	}, 100)

	if !strings.Contains(system, "铁匠鲍勃") || !strings.Contains(system, "固执的铁匠") {
		t.Fatal("提示词应带上角色设定")
	}
	for _, a := range DefaultAllowedActions {
		if !strings.Contains(system, a) {
			t.Fatalf("提示词应列出允许动作 %q", a)
		}
	}
	if !strings.Contains(user, "当前状态") {
		t.Fatal("用户消息应包含状态段")
	}
	if len([]rune(user)) > 200 {
		t.Fatalf("状态文本应被截断，实际 %d 字", len([]rune(user)))
	}
}

// ── 调度器 ──

func newScheduler(t *testing.T, cfg Config, brain Brain, bridge *fakeBridge, clock *fakeClock) *Scheduler {
	t.Helper()
	s, err := New(cfg, brain, map[string]Bridge{"bob": bridge}, WithClock(clock.Now), WithLogger(quietLogger))
	if err != nil {
		t.Fatalf("构造调度器失败: %v", err)
	}
	return s
}

func TestSchedulerRunsAfterInterval(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true, status: "你站在铁匠铺"}
	brain := &fakeBrain{decision: Decision{Action: "wait", Params: map[string]any{"duration": 5}, Reason: "看炉火"}}
	s := newScheduler(t, testConfig(10), brain, bridge, clock)

	// 首轮刻意延后一个周期：刚启动时游戏端可能还没连上
	s.Step(context.Background())
	if bridge.statusIn != 0 {
		t.Fatal("未到期时不应读状态")
	}

	clock.Advance(10 * time.Second)
	s.Step(context.Background())

	if bridge.statusIn != 1 {
		t.Fatalf("到期后应读一次状态，实际 %d", bridge.statusIn)
	}
	if len(brain.requests) != 1 || brain.requests[0].Status != "你站在铁匠铺" {
		t.Fatalf("状态应原样传给决策器，实际 %+v", brain.requests)
	}
	if len(bridge.actArgs) != 1 {
		t.Fatalf("应下发一次动作，实际 %d", len(bridge.actArgs))
	}
	if bridge.actArgs[0]["action"] != "wait" || bridge.actArgs[0]["duration"] != int64(5) {
		t.Fatalf("动作入参不对: %+v", bridge.actArgs[0])
	}

	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Runs != 1 || snap[0].LastAction != "wait" {
		t.Fatalf("快照不对: %+v", snap)
	}
}

func TestSchedulerSkipsOfflineBridge(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: false}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s := newScheduler(t, testConfig(5), brain, bridge, clock)

	clock.Advance(5 * time.Second)
	s.Step(context.Background())

	if bridge.statusIn != 0 || len(brain.requests) != 0 {
		t.Fatal("离线时不应读状态也不应决策")
	}
	snap := s.Snapshot()
	if snap[0].Skips != 1 || snap[0].Runs != 0 {
		t.Fatalf("应记为跳过而不是失败: %+v", snap[0])
	}
}

func TestSchedulerBacksOffOnBrainError(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{err: errors.New("llm timeout")}
	cfg := testConfig(10)
	cfg.FailureCooldownSeconds = 60
	s := newScheduler(t, cfg, brain, bridge, clock)

	clock.Advance(10 * time.Second)
	s.Step(context.Background())

	snap := s.Snapshot()
	if snap[0].Failures != 1 {
		t.Fatalf("应记一次失败，实际 %+v", snap[0])
	}
	// 失败后应退避到 >= 冷却时间，而不是下一个周期又打一次
	wait := snap[0].NextRunAt.Sub(clock.Now())
	if wait < 60*time.Second {
		t.Fatalf("失败后应退避至少 60s，实际 %s", wait)
	}

	// 冷却期内再推进一个周期也不该重试
	clock.Advance(10 * time.Second)
	s.Step(context.Background())
	if len(brain.requests) != 1 {
		t.Fatalf("退避期内不应重试，实际决策 %d 次", len(brain.requests))
	}
}

func TestSchedulerRejectsInvalidAction(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "fly"}}
	s := newScheduler(t, testConfig(5), brain, bridge, clock)

	clock.Advance(5 * time.Second)
	s.Step(context.Background())

	if len(bridge.actArgs) != 0 {
		t.Fatal("非法动作不得下发到客户端")
	}
	snap := s.Snapshot()
	if snap[0].Rejects != 1 || snap[0].Failures != 0 {
		t.Fatalf("非法决策应记为拒绝而不是失败: %+v", snap[0])
	}
	if !strings.Contains(snap[0].LastError, "白名单") {
		t.Fatalf("应记录拒绝原因，实际 %q", snap[0].LastError)
	}
}

func TestSchedulerBacksOffOnActError(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true, actEr: errors.New("reset by peer")}
	brain := &fakeBrain{decision: Decision{Action: "wait", Params: map[string]any{"duration": 1}}}
	cfg := testConfig(5)
	cfg.FailureCooldownSeconds = 30
	s := newScheduler(t, cfg, brain, bridge, clock)

	clock.Advance(5 * time.Second)
	s.Step(context.Background())

	snap := s.Snapshot()
	if snap[0].Failures != 1 {
		t.Fatalf("下发失败应记失败: %+v", snap[0])
	}
	if snap[0].NextRunAt.Sub(clock.Now()) < 30*time.Second {
		t.Fatalf("下发失败后应退避，实际 next=%s", snap[0].NextRunAt)
	}
}

func TestSchedulerSkipsDisabledAgent(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	disabled := false
	cfg := testConfig(5)
	cfg.Agents[0].Enabled = &disabled

	s, err := New(cfg, brain, map[string]Bridge{"bob": bridge}, WithClock(clock.Now), WithLogger(quietLogger))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if s.AgentCount() != 0 {
		t.Fatalf("关闭的 agent 不应进入调度，实际 %d", s.AgentCount())
	}

	clock.Advance(30 * time.Second)
	s.Step(context.Background())
	if bridge.statusIn != 0 {
		t.Fatal("关闭的 agent 不应被驱动")
	}
}

func TestSchedulerMissingBridgeSkips(t *testing.T) {
	clock := newFakeClock()
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s, err := New(testConfig(5), brain, nil, WithClock(clock.Now), WithLogger(quietLogger))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	clock.Advance(5 * time.Second)
	s.Step(context.Background())

	snap := s.Snapshot()
	if snap[0].Skips != 1 {
		t.Fatalf("没有通道的 agent 应记为跳过: %+v", snap[0])
	}
}

// 游戏端明确拒绝（NPC 正在忙/倒地）不是故障：只记拒绝，不推进退避，
// 否则一个正在走长路的 NPC 会把自己的退避拖到十分钟。
func TestSchedulerCountsRejectedActionWithoutBackoff(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true, actEr: fmt.Errorf("%w: npc.act.busy", ErrActionRejected)}
	brain := &fakeBrain{decision: Decision{Action: "wait", Params: map[string]any{"duration": 2}}}
	cfg := testConfig(20)
	cfg.FailureCooldownSeconds = 60
	s := newScheduler(t, cfg, brain, bridge, clock)

	clock.Advance(20 * time.Second)
	s.Step(context.Background())

	snap := s.Snapshot()
	if snap[0].Rejects != 1 {
		t.Fatalf("应记一次拒绝，实际 %+v", snap[0])
	}
	if snap[0].Failures != 0 {
		t.Fatalf("业务拒绝不应计入失败，实际 %+v", snap[0])
	}
	if wait := snap[0].NextRunAt.Sub(clock.Now()); wait != 20*time.Second {
		t.Fatalf("业务拒绝不应退避，下一次应仍是一个周期后，实际 %s", wait)
	}
}

// 读不到状态（NPC 不在场景）按跳过处理，不推进退避
func TestSchedulerSkipsWhenStatusRejected(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true, statusEr: fmt.Errorf("%w: 不在场景", ErrActionRejected)}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s := newScheduler(t, testConfig(10), brain, bridge, clock)

	clock.Advance(10 * time.Second)
	s.Step(context.Background())

	snap := s.Snapshot()
	if snap[0].Skips != 1 || snap[0].Failures != 0 {
		t.Fatalf("状态读取被拒应记为跳过: %+v", snap[0])
	}
	if len(brain.requests) != 0 {
		t.Fatal("读不到状态时不应进入决策")
	}
}

// ── 热更新（控制台改完配置要能生效，否则就是死配置） ──

func TestReloadKeepsRuntimeState(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "wait", Params: map[string]any{"duration": 1}}}
	s := newScheduler(t, testConfig(10), brain, bridge, clock)

	clock.Advance(10 * time.Second)
	s.Step(context.Background())
	if snap := s.Snapshot(); snap[0].Runs != 1 {
		t.Fatalf("前置条件不成立: %+v", snap[0])
	}

	// 只改人格与周期：计数不能被重置，否则每改一次配置 NPC 就重新热身
	next := testConfig(60)
	next.Agents[0].Persona = "改了人格的铁匠"
	if err := s.Reload(next); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	snap := s.Snapshot()
	if snap[0].Runs != 1 {
		t.Fatalf("热更新不应重置运行计数，实际 %+v", snap[0])
	}
	if snap[0].IntervalSec != 60 {
		t.Fatalf("周期应更新为 60s，实际 %d", snap[0].IntervalSec)
	}

	// 新周期生效：推进 10s（旧周期）不应触发，推进到 60s 才触发
	clock.Advance(10 * time.Second)
	s.Step(context.Background())
	if bridge.statusIn != 1 {
		t.Fatalf("旧周期不应再触发，实际读取 %d 次", bridge.statusIn)
	}
}

func TestReloadAddsAndRemovesAgents(t *testing.T) {
	clock := newFakeClock()
	bobBridge := &fakeBridge{online: true}
	daveBridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s, err := New(testConfig(10), brain, map[string]Bridge{"bob": bobBridge, "dave": daveBridge},
		WithClock(clock.Now), WithLogger(quietLogger))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if s.AgentCount() != 1 {
		t.Fatalf("初始应只有 1 个 agent，实际 %d", s.AgentCount())
	}

	withDave := testConfig(10)
	withDave.Agents = append(withDave.Agents, AgentConfig{ID: "dave", Name: "巡守戴夫"})
	if err := s.Reload(withDave); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if s.AgentCount() != 2 {
		t.Fatalf("新增后应有 2 个 agent，实际 %d", s.AgentCount())
	}

	clock.Advance(10 * time.Second)
	s.Step(context.Background())
	if daveBridge.statusIn != 1 {
		t.Fatalf("新增的 agent 应被驱动，实际读取 %d 次", daveBridge.statusIn)
	}

	// 删掉 dave：只保留 bob
	if err := s.Reload(testConfig(10)); err != nil {
		t.Fatalf("热更新失败: %v", err)
	}
	if s.AgentCount() != 1 {
		t.Fatalf("删除后应剩 1 个 agent，实际 %d", s.AgentCount())
	}
}

func TestReloadCanDisableAndReenable(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s := newScheduler(t, testConfig(5), brain, bridge, clock)

	off := testConfig(5)
	off.Enabled = false
	if err := s.Reload(off); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	clock.Advance(30 * time.Second)
	s.Step(context.Background())
	if bridge.statusIn != 0 {
		t.Fatal("总开关关闭时不应驱动任何 agent")
	}

	if err := s.Reload(testConfig(5)); err != nil {
		t.Fatalf("重新开启失败: %v", err)
	}
	clock.Advance(5 * time.Second)
	s.Step(context.Background())
	if bridge.statusIn != 1 {
		t.Fatalf("重新开启后应恢复驱动，实际读取 %d 次", bridge.statusIn)
	}
}

func TestReloadRejectsInvalidConfigAndKeepsOld(t *testing.T) {
	clock := newFakeClock()
	bridge := &fakeBridge{online: true}
	brain := &fakeBrain{decision: Decision{Action: "wait"}}
	s := newScheduler(t, testConfig(10), brain, bridge, clock)

	bad := Config{Enabled: true} // 启用但没有任何 agent
	if err := s.Reload(bad); err == nil {
		t.Fatal("非法配置应返回错误")
	}
	if s.AgentCount() != 1 || !s.Enabled() {
		t.Fatalf("非法配置不应影响原有计划，实际 agents=%d enabled=%v", s.AgentCount(), s.Enabled())
	}
}
