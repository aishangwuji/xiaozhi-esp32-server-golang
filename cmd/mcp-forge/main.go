// mcp-forge 是一个"游戏机制型 MCP 服务"的最小可复制模板。
//
// 为什么要单独起服务，而不是加进服务端本地工具：
//   - 本地工具（RegisterLocalMcpFunc）注册即对全部 NPC 生效，无法按 NPC 分配；
//   - 全局 MCP 服务按 server name 分组，配合 agent 的 mcp_service_names 才能做到
//     "这个 NPC 会打铁，那个 NPC 不会"。
//
// 工具命名约定（重要）：
//
//	本服务内的工具用短名（quote / craft）。服务端聚合时会把键名拼成
//	`<服务名>_<工具名>`（internal/domain/mcp/global_manage.go:625），
//	控制台里服务名填 forge，模型看到的就是 forge_quote / forge_craft。
//	因此：服务名之间不要互为前缀（服务端用 strings.HasPrefix 做白名单匹配）。
//
// 用法：
//
//	go run ./cmd/mcp-forge -selfcheck                 # 不开端口，跑一遍两个工具
//	go run ./cmd/mcp-forge -addr :18081               # 同时挂 /sse、/message、/mcp
//	go run ./cmd/mcp-forge -addr :18081 -transport sse
//
// 控制台登记：
//
//	MCP 配置 → 新增服务，name=forge，type=sse，url=http://<host>:18081/sse
//	或 type=streamablehttp，url=http://<host>:18081/mcp
package main

import (
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const (
	serverName    = "mcp-forge"
	serverVersion = "0.1.0"
)

// craftItems 机制内合法的物品类型。真实项目改成从配置/数据库读。
var craftItems = []string{"sword", "shield", "ring", "helm"}

func main() {
	addr := flag.String("addr", ":18081", "监听地址")
	transport := flag.String("transport", "both", "both|sse|streamable")
	selfcheck := flag.Bool("selfcheck", false, "只跑工具自检，不监听端口")
	flag.Parse()

	mcpServer := buildServer()

	if *selfcheck {
		if err := runSelfcheck(mcpServer); err != nil {
			fmt.Fprintf(os.Stderr, "selfcheck 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("selfcheck OK: quote / craft 均返回预期结构")
		return
	}

	mux := http.NewServeMux()
	sseServer := server.NewSSEServer(mcpServer)
	streamServer := server.NewStreamableHTTPServer(mcpServer)

	if *transport == "both" || *transport == "sse" {
		// SSE 服务端按精确路径匹配，两个端点都要挂
		mux.Handle("/sse", sseServer)
		mux.Handle("/message", sseServer)
		log.Printf("SSE      http://%s/sse", normalizeAddr(*addr))
	}
	if *transport == "both" || *transport == "streamable" {
		mux.Handle("/mcp", streamServer)
		log.Printf("HTTP     http://%s/mcp", normalizeAddr(*addr))
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("mcp-forge 启动，transport=%s addr=%s", *transport, *addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("监听失败: %v", err)
	}
}

func normalizeAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "127.0.0.1" + addr
	}
	return addr
}

func buildServer() *server.MCPServer {
	s := server.NewMCPServer(serverName, serverVersion,
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	s.AddTool(
		mcp.NewTool("quote",
			mcp.WithDescription("查询锻造报价。当玩家问某件装备多少钱、值不值得打、要多少材料时使用。返回金币价格与所需材料数。"),
			mcp.WithString("item",
				mcp.Required(),
				mcp.Description("装备类型，可选 sword / shield / ring / helm"),
				mcp.WithStringEnumItems(craftItems),
			),
			mcp.WithNumber("tier",
				mcp.Description("品阶，1~5，越大越贵；缺省为 1"),
				mcp.Min(1),
				mcp.Max(5),
			),
		),
		handleQuote,
	)

	s.AddTool(
		mcp.NewTool("craft",
			mcp.WithDescription("下单锻造一件装备。玩家明确表示要打造、锻造、做一件东西时使用。会扣金币与材料，可能失败。"),
			mcp.WithString("item",
				mcp.Required(),
				mcp.Description("装备类型，可选 sword / shield / ring / helm"),
				mcp.WithStringEnumItems(craftItems),
			),
			mcp.WithNumber("tier",
				mcp.Description("品阶，1~5，缺省为 1"),
				mcp.Min(1),
				mcp.Max(5),
			),
			mcp.WithNumber("materials",
				mcp.Description("玩家愿意投入的材料数量，影响成功率；缺省为报价材料数"),
				mcp.Min(0),
			),
		),
		handleCraft,
	)

	return s
}

type quote struct {
	Item      string `json:"item"`
	Tier      int    `json:"tier"`
	Price     int    `json:"price_gold"`
	Materials int    `json:"materials_required"`
	Note      string `json:"note"`
}

func computeQuote(item string, tier int) quote {
	if tier < 1 {
		tier = 1
	}
	if tier > 5 {
		tier = 5
	}
	base := 120 + stableJitter(item, 80) // 120~200
	q := quote{
		Item:      item,
		Tier:      tier,
		Price:     base * tier,
		Materials: 2 + tier,
	}
	q.Note = fmt.Sprintf("%s（%d 阶）报价 %d 金币，需 %d 份材料", item, tier, q.Price, q.Materials)
	return q
}

func handleQuote(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	item, err := req.RequireString("item")
	if err != nil {
		return mcp.NewToolResultError("item 不能为空"), nil
	}
	if !validItem(item) {
		return mcp.NewToolResultErrorf("不支持的装备类型 %q，可选：%v", item, craftItems), nil
	}
	tier := req.GetInt("tier", 1)
	q := computeQuote(item, tier)
	return mcp.NewToolResultText(fmt.Sprintf(
		`{"item":%q,"tier":%d,"price_gold":%d,"materials_required":%d,"note":%q}`,
		q.Item, q.Tier, q.Price, q.Materials, q.Note,
	)), nil
}

func handleCraft(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	item, err := req.RequireString("item")
	if err != nil {
		return mcp.NewToolResultError("item 不能为空"), nil
	}
	if !validItem(item) {
		return mcp.NewToolResultErrorf("不支持的装备类型 %q，可选：%v", item, craftItems), nil
	}
	tier := req.GetInt("tier", 1)
	q := computeQuote(item, tier)
	materials := req.GetInt("materials", q.Materials)
	if materials < 0 {
		materials = 0
	}

	// 成功率 = 基础 55% + 每多投入 1 份材料 +10%，上限 95%
	rate := 55 + (materials-q.Materials)*10
	if rate < 10 {
		rate = 10
	}
	if rate > 95 {
		rate = 95
	}
	roll := stableJitter(item+fmt.Sprintf("%d", tier), 100)
	ok := roll < rate

	status := "failed"
	result := fmt.Sprintf("%s（%d 阶）锻造失败，材料已消耗，铁匠让你下次多备些料", item, tier)
	if ok {
		status = "succeeded"
		result = fmt.Sprintf("%s（%d 阶）锻造成功，已放入背包", item, tier)
	}
	return mcp.NewToolResultText(fmt.Sprintf(
		`{"status":%q,"item":%q,"tier":%d,"price_gold":%d,"materials_used":%d,"success_rate":%d,"result":%q}`,
		status, item, tier, q.Price, materials, rate, result,
	)), nil
}

func validItem(item string) bool {
	for _, v := range craftItems {
		if v == item {
			return true
		}
	}
	return false
}

// stableJitter 同一输入恒定返回 [0,n) 的伪随机数，避免测试与复现时结果漂移。
func stableJitter(seed string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	return int(h.Sum32() % uint32(n))
}

// runSelfcheck 直接调用工具实现，验证成功路径与错误路径，不依赖网络与端口。
func runSelfcheck(_ *server.MCPServer) error {
	ctx := context.Background()

	cases := []struct {
		label   string
		handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
		args    map[string]any
		want    string
	}{
		{"quote 正常", handleQuote, map[string]any{"item": "sword", "tier": 3}, "materials_required"},
		{"craft 正常", handleCraft, map[string]any{"item": "ring", "tier": 2, "materials": 6}, "status"},
		{"quote 缺参", handleQuote, map[string]any{}, "item 不能为空"},
		{"quote 非法物品", handleQuote, map[string]any{"item": "spaceship"}, "不支持的装备类型"},
		{"craft 非法物品", handleCraft, map[string]any{"item": "spaceship"}, "不支持的装备类型"},
	}

	for _, c := range cases {
		req := mcp.CallToolRequest{}
		req.Params.Arguments = c.args

		res, err := c.handler(ctx, req)
		if err != nil {
			return fmt.Errorf("%s: %w", c.label, err)
		}
		text := resultText(res)
		if !strings.Contains(text, c.want) {
			return fmt.Errorf("%s: 期望包含 %q，实际 %q", c.label, c.want, text)
		}
	}
	return nil
}

func resultText(res *mcp.CallToolResult) string {
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return fmt.Sprintf("%v", res.Content[0])
}
