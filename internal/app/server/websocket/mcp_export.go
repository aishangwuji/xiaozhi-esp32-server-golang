package websocket

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	tool "github.com/cloudwego/eino/components/tool"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	domainmcp "xiaozhi-esp32-server-golang/internal/domain/mcp"
	log "xiaozhi-esp32-server-golang/logger"
)

// MCPExportServer 把本服务按 agent 聚合的工具（含游戏经 /mcp 端点注册的工具）以标准
// MCP 协议（SSE 传输）导出给外部平台（小智官方 agent 的"自定义 MCP"）调用。
//
// 路由（挂在主 mux 上，前缀 /mcp_export/）：
//
//	GET  /mcp_export/{agentId}/sse?token=<agentJWT>   SSE 事件流（首个事件下发 message 端点）
//	POST /mcp_export/{agentId}/message?token=<agentJWT>&sessionId=xxx  JSON-RPC 消息入口
//
// 协议层复用 mark3labs/mcp-go 的 MCPServer.HandleMessage（initialize/tools/list/tools/call
// 的协议正确性由其保证）；工具集在会话建立时按 agentId 实时聚合——工具变更靠外部平台
// 重连刷新（与 xiaozhi 官方对自定义 MCP 的会话生命周期一致）。
type MCPExportServer struct {
	mu      sync.Mutex
	streams map[string]*mcpExportStream
}

// 包级单例：SSE 会话表必须在 SSE 与 message 两个请求间共享
var mcpExportHub = NewMCPExportServer()

type mcpExportStream struct {
	agentId string
	server  *mcpserver.MCPServer
	events  chan string
	closed  chan struct{}
	once    sync.Once
}

func NewMCPExportServer() *MCPExportServer {
	return &MCPExportServer{
		streams: make(map[string]*mcpExportStream),
	}
}

// parseExportToken 校验导出端点的 agent JWT（与 /mcp 端点同一签名体系）
func parseExportToken(tokenString string) (*MCPClaims, error) {
	return (&WebSocketServer{}).parseMCPToken(tokenString)
}

func (s *MCPExportServer) handleMCPExport(w http.ResponseWriter, r *http.Request) {
	// 路径: /mcp_export/{agentId}/{sse|message}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "mcp_export" {
		http.Error(w, "bad path", http.StatusNotFound)
		return
	}
	agentId, action := parts[1], parts[2]

	claims, err := parseExportToken(r.URL.Query().Get("token"))
	if err != nil || claims.AgentID != agentId {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	switch action {
	case "sse":
		s.handleSSE(w, r, agentId)
	case "message":
		s.handleMessage(w, r, agentId)
	default:
		http.Error(w, "bad action", http.StatusNotFound)
	}
}

// GET .../sse：建立 SSE 会话，先下发 message 端点事件
func (s *MCPExportServer) handleSSE(w http.ResponseWriter, r *http.Request, agentId string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 会话级 MCPServer：工具集建立时按 agent 实时聚合（tools/call 路由到设备桥接）
	tools, err := domainmcp.GetToolsByDeviceIdWithTransport("", agentId, "mcp_export", "")
	if err != nil {
		log.Errorf("MCP导出: 聚合 agent %s 工具失败: %v", agentId, err)
		http.Error(w, "tool aggregation failed", http.StatusInternalServerError)
		return
	}
	mcpSrv := s.buildAgentMCPServer(agentId, tools)

	sessionId := uuid.NewString()
	stream := &mcpExportStream{
		agentId: agentId,
		server:  mcpSrv,
		events:  make(chan string, 64),
		closed:  make(chan struct{}),
	}
	s.mu.Lock()
	s.streams[sessionId] = stream
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, sessionId)
		s.mu.Unlock()
		close(stream.closed)
	}()
	log.Infof("MCP导出: agent=%s 会话=%s 已建立（%d 个工具）", agentId, sessionId, len(tools))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	fmt.Fprintf(w, "event: endpoint\ndata: /mcp_export/%s/message?token=%s&sessionId=%s\n\n",
		agentId, r.URL.Query().Get("token"), sessionId)
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-stream.closed:
			return
		case evt := <-stream.events:
			fmt.Fprintf(w, "%s", evt)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// POST .../message：JSON-RPC 入口，响应经 SSE 推回
func (s *MCPExportServer) handleMessage(w http.ResponseWriter, r *http.Request, agentId string) {
	sessionId := r.URL.Query().Get("sessionId")
	s.mu.Lock()
	stream := s.streams[sessionId]
	s.mu.Unlock()
	if stream == nil || stream.agentId != agentId {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	go func(stream *mcpExportStream, body json.RawMessage) {
		// 协议层交给 mcp-go：initialize/tools/list/tools/call 全由其处理；
		// tools/call 最终路由到设备桥接（游戏经 /mcp 端点注册的工具）
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		resp := stream.server.HandleMessage(ctx, body)
		if resp == nil {
			return // 通知类消息无需响应
		}
		data, err := json.Marshal(resp)
		if err != nil {
			log.Errorf("MCP导出: 响应序列化失败: %v", err)
			return
		}
		select {
		case stream.events <- fmt.Sprintf("event: message\ndata: %s\n\n", data):
		case <-stream.closed:
		}
	}(stream, body)

	w.WriteHeader(http.StatusAccepted)
}

// 把按 agent 聚合的工具集包装成 mcp-go 的 MCPServer 实例
func (s *MCPExportServer) buildAgentMCPServer(agentId string, tools map[string]tool.InvokableTool) *mcpserver.MCPServer {
	mcpSrv := mcpserver.NewMCPServer("lingxi-agent-"+agentId, "1.0.0",
		mcpserver.WithInstructions("灵起（LingXi）游戏 NPC 的业务工具。调用工具会直接在玩家面前产生游戏内效果（打开商店、接取任务等）。"))
	for _, t := range tools {
		info, infoErr := t.Info(context.Background())
		if infoErr != nil || info == nil {
			log.Warnf("MCP导出: agent=%s 跳过无 Info 的工具", agentId)
			continue
		}
		// InputSchema 固定宽松 object：参数语义靠 description 约定（工具侧自行解析 arguments）
		mcpTool := mcp.NewTool(info.Name, mcp.WithDescription(info.Desc))
		fn := t
		mcpSrv.AddTool(mcpTool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			argsJSON, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return mcp.NewToolResultError("参数序列化失败"), nil
			}
			out, err := fn.InvokableRun(ctx, string(argsJSON))
			if err != nil {
				log.Warnf("MCP导出: agent=%s 工具 %s 调用失败: %v", agentId, info.Name, err)
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText(out), nil
		})
	}
	return mcpSrv
}
