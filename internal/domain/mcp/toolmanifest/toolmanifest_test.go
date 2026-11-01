package toolmanifest

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// stubInvokableTool 测试桩：可配置 Info 与执行透传记录
type stubInvokableTool struct {
	name    string
	desc    string
	infoErr error
	lastRun string
}

func (s *stubInvokableTool) Info(context.Context) (*schema.ToolInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return &schema.ToolInfo{
		Name: s.name,
		Desc: s.desc,
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"request_text": {Type: schema.String, Desc: "原参数"},
		}),
	}, nil
}

func (s *stubInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	s.lastRun = argumentsInJSON
	return "ok", nil
}

// 断言先行（L2）：覆盖合并的核心行为在这里锁定
func TestApply_DescriptionOnly(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", desc: "本地描述"}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{
		"npc_trade": {ToolName: "npc_trade", Description: "云端描述", Enabled: true},
	}

	result := Apply(tools, overrides)
	got, ok := result["npc_trade"]
	if !ok {
		t.Fatal("工具不应被摘除")
	}
	info, err := got.Info(context.Background())
	if err != nil {
		t.Fatalf("Info 不应报错: %v", err)
	}
	if info.Desc != "云端描述" {
		t.Fatalf("描述应被云端覆盖, got=%q", info.Desc)
	}
	if info.Name != "npc_trade" {
		t.Fatalf("工具名必须保持稳定(执行派发键), got=%q", info.Name)
	}
	// 执行路径必须原样委托原工具
	if _, err := got.InvokableRun(context.Background(), `{"request_text":"买药"}`); err != nil {
		t.Fatalf("InvokableRun 不应报错: %v", err)
	}
	if inner.lastRun != `{"request_text":"买药"}` {
		t.Fatal("执行参数未透传到原工具")
	}
}

func TestApply_SchemaOverride(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", desc: "d"}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{
		"npc_trade": {
			ToolName:    "npc_trade",
			InputSchema: `{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`,
			Enabled:     true,
		},
	}

	result := Apply(tools, overrides)
	info, err := result["npc_trade"].Info(context.Background())
	if err != nil {
		t.Fatalf("Info 不应报错: %v", err)
	}
	params, err := info.ParamsOneOf.ToOpenAPIV3()
	if err != nil || params == nil {
		t.Fatalf("入参 schema 应可解析: err=%v params=%v", err, params)
	}
	if _, ok := params.Properties["count"]; !ok {
		t.Fatal("入参 schema 应被云端版本覆盖(应含 count)")
	}
	if _, ok := params.Properties["request_text"]; ok {
		t.Fatal("入参 schema 应被云端版本覆盖(不应残留 request_text)")
	}
}

func TestApply_DisabledRemovesTool(t *testing.T) {
	trade := &stubInvokableTool{name: "npc_trade", desc: "d"}
	quest := &stubInvokableTool{name: "npc_quest_accept", desc: "d"}
	tools := map[string]tool.InvokableTool{"npc_trade": trade, "npc_quest_accept": quest}
	overrides := map[string]ToolOverride{"npc_trade": {ToolName: "npc_trade", Enabled: false}}

	result := Apply(tools, overrides)
	if _, exists := result["npc_trade"]; exists {
		t.Fatal("enabled=false 的工具应被摘除")
	}
	if _, exists := result["npc_quest_accept"]; !exists {
		t.Fatal("未列出的工具应保留")
	}
	// 合并不得修改入参（聚合结果可能被调用方继续使用）
	if _, exists := tools["npc_trade"]; !exists {
		t.Fatal("合并逻辑不得修改入参 map")
	}
}

func TestApply_UnknownToolIgnored(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", desc: "d"}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{"npc_missing": {ToolName: "npc_missing", Description: "x", Enabled: true}}

	result := Apply(tools, overrides)
	if len(result) != 1 {
		t.Fatalf("未知工具名不得新增工具条目, got=%d", len(result))
	}
	if _, exists := result["npc_missing"]; exists {
		t.Fatal("不得新增设备侧未上报的工具（can_offer 是可用性唯一裁决）")
	}
}

func TestApply_EmptyFieldsKeepOriginal(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", desc: "本地描述"}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{"npc_trade": {ToolName: "npc_trade", Enabled: true}}

	result := Apply(tools, overrides)
	if result["npc_trade"] != tool.InvokableTool(inner) {
		t.Fatal("description/input_schema 均空时应原样返回原工具（零开销路径）")
	}
}

func TestApply_InvalidSchemaKeepsOriginal(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", desc: "本地描述"}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{
		"npc_trade": {ToolName: "npc_trade", InputSchema: "{不是json", Enabled: true},
	}

	result := Apply(tools, overrides)
	info, err := result["npc_trade"].Info(context.Background())
	if err != nil {
		t.Fatalf("原工具 Info 不应报错: %v", err)
	}
	if info.Desc != "本地描述" {
		t.Fatal("schema 解析失败时应保留原工具原信息")
	}
}

func TestApply_InfoErrorKeepsOriginal(t *testing.T) {
	inner := &stubInvokableTool{name: "npc_trade", infoErr: errors.New("info boom")}
	tools := map[string]tool.InvokableTool{"npc_trade": inner}
	overrides := map[string]ToolOverride{
		"npc_trade": {ToolName: "npc_trade", Description: "云端描述", Enabled: true},
	}

	result := Apply(tools, overrides)
	if _, exists := result["npc_trade"]; !exists {
		t.Fatal("Info 失败的工具应保留原工具而非摘除")
	}
}

func TestApply_EmptyInputsShortCircuit(t *testing.T) {
	if got := Apply(nil, map[string]ToolOverride{"a": {Enabled: true}}); got != nil {
		t.Fatal("空工具表应原样返回")
	}
	tools := map[string]tool.InvokableTool{"npc_trade": &stubInvokableTool{name: "npc_trade"}}
	if got := Apply(tools, nil); len(got) != 1 {
		t.Fatal("空清单应原样返回")
	}
}

// IndexOverrides 边界校验：空名条目丢弃
func TestIndexOverrides_SkipsEmptyName(t *testing.T) {
	indexed := IndexOverrides([]ToolOverride{
		{ToolName: "", Enabled: true},
		{ToolName: "npc_trade", Enabled: false},
	})
	if len(indexed) != 1 {
		t.Fatalf("空名条目应被丢弃, got=%d", len(indexed))
	}
	if _, ok := indexed["npc_trade"]; !ok {
		t.Fatal("有效条目应被索引")
	}
}
