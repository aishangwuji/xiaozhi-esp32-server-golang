package controllers

import "testing"

// 断言先行（L2）：清单写入的边界校验行为在这里锁定
func TestValidateNpcToolManifestInput(t *testing.T) {
	longSchema := `{"type":"object","properties":{` + string(make([]byte, 8200)) + `}}`

	cases := []struct {
		name    string
		req     npcToolManifestRequest
		wantErr bool
	}{
		{
			name: "agent专属条目_合法",
			req:  npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", Description: "买卖物品", Enabled: boolPtr(true)},
		},
		{
			name: "全局条目_agent_id为空合法",
			req:  npcToolManifestRequest{AgentID: "", ToolName: "npc_trade", Description: "全局默认"},
		},
		{
			name:    "tool_name必填",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "  "},
			wantErr: true,
		},
		{
			name:    "input_schema必须为JSON对象_数组拒绝",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", InputSchema: `[]`},
			wantErr: true,
		},
		{
			name:    "input_schema必须为JSON对象_标量拒绝",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", InputSchema: `"x"`},
			wantErr: true,
		},
		{
			name:    "input_schema非法JSON拒绝",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", InputSchema: `{bad json`},
			wantErr: true,
		},
		{
			name:    "input_schema超长拒绝",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", InputSchema: longSchema},
			wantErr: true,
		},
		{
			name:    "agent_id超长拒绝",
			req:     npcToolManifestRequest{AgentID: string(make([]byte, 65)), ToolName: "npc_trade"},
			wantErr: true,
		},
		{
			name:    "description超长拒绝",
			req:     npcToolManifestRequest{AgentID: "3", ToolName: "npc_trade", Description: string(make([]byte, 4001))},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, errMsg := validateNpcToolManifestInput(&tc.req)
			if (errMsg != "") != tc.wantErr {
				t.Fatalf("wantErr=%v, got errMsg=%q", tc.wantErr, errMsg)
			}
		})
	}
}

func boolPtr(v bool) *bool {
	return &v
}
