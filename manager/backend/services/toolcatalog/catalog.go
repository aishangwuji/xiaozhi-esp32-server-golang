// Package toolcatalog 提供 NPC 工具目录的"声明式快照"。
//
// 为什么需要它：
//
//	  manager/backend 是独立 Go module（module xiaozhi/manager/backend），
//	  无法 import 主服务的 internal/domain/mcp，因此拿不到运行时注册的本地工具清单。
//	  本地工具与固件工具是"代码里固定存在"的集合，用构建期快照 + 一致性校验更可靠：
//
//		生成：python esp32-watch/tools/tool_inventory.py --emit-catalog <本目录>/local_tools.json
//		校验：python esp32-watch/tools/tool_inventory.py --check-catalog <本目录>/local_tools.json
//
// 快照只用于"目录可见性/命中率"展示，不参与工具注册或执行，改了它不会影响线上行为。
package toolcatalog

import (
	_ "embed"
	"encoding/json"
	"sync"
)

//go:embed local_tools.json
var embedded []byte

// Tool 单个工具的声明信息
type Tool struct {
	Name        string `json:"name"`
	Source      string `json:"source"` // local=服务端本地工具，firmware=固件设备侧工具
	File        string `json:"file"`
	Desc        string `json:"desc"`
	Conditional bool   `json:"conditional"` // 固件侧条件编译项，实际是否存在取决于板卡配置
}

// Catalog 快照内容
type Catalog struct {
	Board string `json:"board"`
	Note  string `json:"note"`
	Tools []Tool `json:"tools"`
}

var (
	once    sync.Once
	cached  Catalog
	loadErr error
)

// Get 返回解析后的快照（只解析一次）
func Get() (Catalog, error) {
	once.Do(func() {
		loadErr = json.Unmarshal(embedded, &cached)
	})
	return cached, loadErr
}
