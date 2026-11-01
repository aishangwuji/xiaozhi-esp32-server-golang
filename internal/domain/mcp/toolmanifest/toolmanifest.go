// Package toolmanifest NPC 工具清单覆盖的纯合并逻辑（工具描述服务端化·阶段一）。
//
// 独立成包的原因：不携带音频(opus)/sonic 等 cgo 依赖链，核心合并行为可在任意环境单测锁定。
//
// 红线约束：
//   - 目录级调整：不新增设备侧未上报的工具（客户端 can_offer 仍是执行可达性的唯一裁决）；
//   - 不参与意图判定：覆盖只改变 LLM 看到的工具描述/入参 schema，执行路径原样委托原工具；
//   - 任何单条覆盖应用失败都保留原工具，绝不因清单数据问题把工具弄丢（enabled=false 除外）。
package toolmanifest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	log "xiaozhi-esp32-server-golang/logger"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/getkin/kin-openapi/openapi3"
)

// ToolOverride 单个工具的云端覆盖项（与 manager 控制台 npc_tool_manifest 表字段对应）
type ToolOverride struct {
	ToolName    string `json:"tool_name"`    // 工具名，与设备侧上报的工具名严格一致（执行派发键）
	Description string `json:"description"`  // 空串=不覆盖描述
	InputSchema string `json:"input_schema"` // 空串=不覆盖入参 schema（openapi3 JSON 对象文本）
	Enabled     bool   `json:"enabled"`      // false=从聚合结果中摘除（云端总开关）
}

// IndexOverrides 按工具名建索引；空名条目在边界处丢弃
func IndexOverrides(list []ToolOverride) map[string]ToolOverride {
	indexed := make(map[string]ToolOverride, len(list))
	for _, ov := range list {
		name := strings.TrimSpace(ov.ToolName)
		if name == "" {
			continue
		}
		indexed[name] = ov
	}
	return indexed
}

// overrideInvokableTool 元数据覆盖包装器：Info 返回云端目录版本，执行原样委托原工具
type overrideInvokableTool struct {
	inner tool.InvokableTool
	info  *schema.ToolInfo
}

func (t *overrideInvokableTool) Info(context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *overrideInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	return t.inner.InvokableRun(ctx, argumentsInJSON, opts...)
}

// wrapWithOverrideInfo 按覆盖项生成包装工具；description/input_schema 均空时无需包装
func wrapWithOverrideInfo(inner tool.InvokableTool, ov ToolOverride) (tool.InvokableTool, error) {
	if ov.Description == "" && ov.InputSchema == "" {
		return inner, nil
	}
	base, err := inner.Info(context.Background())
	if err != nil {
		return nil, fmt.Errorf("获取原工具 Info 失败: %w", err)
	}
	if base == nil {
		return nil, fmt.Errorf("原工具 Info 为空")
	}
	info := *base // 浅拷贝安全：ToolInfo 仅含值字段与一个指针字段
	if ov.Description != "" {
		info.Desc = ov.Description
	}
	if ov.InputSchema != "" {
		parsed := &openapi3.Schema{}
		if err := json.Unmarshal([]byte(ov.InputSchema), parsed); err != nil {
			return nil, fmt.Errorf("input_schema 解析失败: %w", err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByOpenAPIV3(parsed)
	}
	return &overrideInvokableTool{inner: inner, info: &info}, nil
}

// Apply 纯合并逻辑：返回新 map，不修改入参。
// 未知工具名忽略（只覆盖设备侧已上报的工具，不新增）；enabled=false 摘除。
func Apply(tools map[string]tool.InvokableTool, overrides map[string]ToolOverride) map[string]tool.InvokableTool {
	if len(overrides) == 0 || len(tools) == 0 {
		return tools
	}
	result := make(map[string]tool.InvokableTool, len(tools))
	for name, t := range tools {
		result[name] = t
	}
	for name, ov := range overrides {
		inner, exists := tools[name]
		if !exists {
			continue
		}
		if !ov.Enabled {
			delete(result, name)
			continue
		}
		wrapped, err := wrapWithOverrideInfo(inner, ov)
		if err != nil {
			// 保留原工具：清单数据质量问题不允许造成工具丢失
			log.Warnf("应用工具清单覆盖失败(保留原工具): tool=%s err=%v", name, err)
			continue
		}
		result[name] = wrapped
	}
	return result
}
