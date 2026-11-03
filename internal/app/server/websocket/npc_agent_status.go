package websocket

// npc_agent_status.go — NPC 常驻 Agent 的只读状态接口（管理端消费）
//
// 只读、只暴露调度器内存快照，不触发任何决策或动作。
// 鉴权：与控制台→主服务的其它内部调用共用 manager 内部 token（Authorization: Bearer <token>）。
// nginx 只把 /xiaozhi/、/ws/、/mcp、/mcp_export/ 反代到本端口，因此 /internal/ 不会暴露到公网。

import (
	"encoding/json"
	"net/http"
	"strings"

	"xiaozhi-esp32-server-golang/internal/util"
	log "xiaozhi-esp32-server-golang/logger"
)

// npcAgentStatusProvider 返回可序列化的调度器快照；未启用时也要返回结构（enabled=false），
// 让控制台能区分"没开"和"坏了"。
type npcAgentStatusProvider func() any

// WithNpcAgentStatusProvider 注入状态提供者
func WithNpcAgentStatusProvider(provider npcAgentStatusProvider) WebSocketServerOption {
	return func(s *WebSocketServer) {
		s.npcAgentStatusProvider = provider
	}
}

func (s *WebSocketServer) handleNpcAgentStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	expected := strings.TrimSpace(util.GetManagerAuthToken())
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	token := ""
	if strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	}
	if expected == "" || token != expected {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if s.npcAgentStatusProvider == nil {
		http.Error(w, "npc agent status unavailable", http.StatusServiceUnavailable)
		return
	}

	payload := s.npcAgentStatusProvider()
	writeJSON(w, payload)
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body, err := json.Marshal(payload)
	if err != nil {
		log.Errorf("[npcagent] 序列化状态失败: %v", err)
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Warnf("[npcagent] 写状态响应失败: %v", err)
	}
}
