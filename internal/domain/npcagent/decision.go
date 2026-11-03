package npcagent

// decision.go — 一次决策的契约：提示词构造、模型输出解析、参数校验
//
// 红线：本文件只定义"模型能说什么动作、参数长什么样"，不猜测玩家意图，
// 也不做任何"看到某关键词就选某动作"的兜底。模型输出不合法就丢弃本次决策，
// 由下一个周期重来。

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// DecideRequest 交给决策器（LLM）的输入
type DecideRequest struct {
	AgentID string
	Name    string
	Persona string
	// Status 客户端 npc_status 返回的状态快照文本
	Status string
	// Allowed 允许的动作 id
	Allowed []string
}

// Decision 模型给出的动作意图
type Decision struct {
	Action string
	Params map[string]any
	Reason string
}

// 每个动作允许携带的参数键。未列出的键一律丢弃：模型凭空造参数不应到达游戏客户端。
var allowedParamKeys = map[string]map[string]struct{}{
	"walk":   {"location": {}, "x": {}, "z": {}, "hold_seconds": {}, "say": {}},
	"run":    {"location": {}, "x": {}, "z": {}, "hold_seconds": {}, "say": {}},
	"jump":   {"force": {}},
	"talk":   {"target": {}, "say": {}, "duration": {}},
	"attack": {"target": {}, "duration": {}},
	"wait":   {"duration": {}, "say": {}},
	"stop":   {},
}

// actionHelp 拼进提示词的动作参数说明
const actionHelp = `动作参数：
- walk / run：location（地点名）或 x、z（世界坐标，必须同时给出）；hold_seconds 抵达后停留秒数
- jump：无参数
- talk：target（NPC 的 id 或 player）、say（一句话台词）、duration 秒数
- attack：target、duration 秒数
- wait：duration 秒数，可带 say
- stop：无参数`

// BuildPrompt 生成决策提示词。status 会按 maxStatusChars 截断，避免把整份快照塞进上下文。
func BuildPrompt(req DecideRequest, maxStatusChars int) (system string, user string) {
	system = strings.TrimSpace(fmt.Sprintf(`你是「%s」，%s

你在一个开放世界里控制这个角色的身体。每隔一段时间你有一次行动机会，要自己决定这下做什么。
可选动作：%s

%s

规则：
1. 只输出一个 JSON 对象，形如 {"action":"wait","params":{},"reason":"一句话说明"}
2. 不要输出任何多余文字，不要用 markdown 代码块
3. 只能用上面列出的动作与参数；拿不准时就选 wait
4. 地点名、人物只能来自下面「当前状态」里出现过的信息，不要编造不存在的地方或人`,
		fallback(strings.TrimSpace(req.Name), req.AgentID),
		fallback(strings.TrimSpace(req.Persona), "一个普通的城镇居民"),
		strings.Join(req.Allowed, " / "),
		actionHelp))

	status := strings.TrimSpace(req.Status)
	if maxStatusChars > 0 {
		status = truncateRunes(status, maxStatusChars)
	}
	if status == "" {
		status = "（暂时读不到状态，按 wait 处理）"
	}
	user = "当前状态：\n" + status + "\n\n现在轮到你了，输出你的决定。"
	return system, user
}

// ParseDecision 从模型输出里取出决策 JSON。
// 容忍代码块包裹与前后多余文字，但只认第一个完整 JSON 对象。
func ParseDecision(raw string) (Decision, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return Decision{}, fmt.Errorf("模型输出为空")
	}
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")

	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return Decision{}, fmt.Errorf("模型输出里没有 JSON 对象")
	}
	payload := text[start : end+1]

	// params 里的数字默认是 float64；用 UseNumber 保留整数语义，避免 x/z 变成 12.0000001
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.UseNumber()
	var parsed struct {
		Action string          `json:"action"`
		Params json.RawMessage `json:"params"`
		Reason string          `json:"reason"`
	}
	if err := dec.Decode(&parsed); err != nil {
		return Decision{}, fmt.Errorf("解析决策 JSON 失败: %w", err)
	}

	d := Decision{Action: strings.ToLower(strings.TrimSpace(parsed.Action)), Reason: strings.TrimSpace(parsed.Reason)}
	if len(parsed.Params) > 0 {
		var params map[string]any
		pdec := json.NewDecoder(strings.NewReader(string(parsed.Params)))
		pdec.UseNumber()
		if err := pdec.Decode(&params); err != nil {
			return Decision{}, fmt.Errorf("解析决策参数失败: %w", err)
		}
		d.Params = params
	}
	if d.Params == nil {
		d.Params = map[string]any{}
	}
	return d, nil
}

// Validate 校验并规范化决策：动作必须在白名单内，参数按键白名单过滤并截断。
func (d *Decision) Validate(allowed []string, maxReasonChars int) error {
	if d.Action == "" {
		return fmt.Errorf("缺少 action")
	}
	if !contains(allowed, d.Action) {
		return fmt.Errorf("动作不在白名单内: %s", d.Action)
	}
	if d.Params == nil {
		d.Params = map[string]any{}
	}

	keys := allowedParamKeys[d.Action]
	if keys == nil {
		return fmt.Errorf("未登记参数契约的动作: %s", d.Action)
	}
	for key := range d.Params {
		if _, ok := keys[key]; !ok {
			delete(d.Params, key)
		}
	}

	// 统一把数字收敛成 JSON 友好的 int/float，字符串去掉空白并截断
	for key, v := range d.Params {
		switch val := v.(type) {
		case json.Number:
			if i, err := val.Int64(); err == nil {
				d.Params[key] = i
			} else if f, err := val.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				d.Params[key] = f
			} else {
				delete(d.Params, key)
			}
		case string:
			d.Params[key] = truncateRunes(strings.TrimSpace(val), 200)
		case int:
			d.Params[key] = int64(val)
		case int64:
			d.Params[key] = val
		case float64:
			if math.IsNaN(val) || math.IsInf(val, 0) {
				delete(d.Params, key)
				continue
			}
			d.Params[key] = val
		case bool:
			d.Params[key] = val
		case nil:
			delete(d.Params, key)
		default:
			// 嵌套对象/数组不接受：动作参数是扁平契约
			delete(d.Params, key)
		}
	}

	switch d.Action {
	case "walk", "run":
		_, hasLoc := d.Params["location"]
		_, hasX := d.Params["x"]
		_, hasZ := d.Params["z"]
		if !hasLoc && !(hasX && hasZ) {
			return fmt.Errorf("walk/run 需要 location 或同时给出 x、z")
		}
		if hasLoc && strings.TrimSpace(fmt.Sprint(d.Params["location"])) == "" {
			return fmt.Errorf("location 为空")
		}
	case "talk", "attack":
		target, ok := d.Params["target"]
		if !ok || strings.TrimSpace(fmt.Sprint(target)) == "" {
			d.Params["target"] = "player"
		}
	}

	d.Reason = truncateRunes(d.Reason, maxReasonChars)
	return nil
}

// ActArgs 把决策转成 npc_act 工具入参（扁平结构，action 与参数同级）
func (d Decision) ActArgs() map[string]any {
	args := make(map[string]any, len(d.Params)+1)
	args["action"] = d.Action
	for k, v := range d.Params {
		args[k] = v
	}
	return args
}

func contains(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func fallback(val, def string) string {
	if strings.TrimSpace(val) == "" {
		return def
	}
	return val
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}
