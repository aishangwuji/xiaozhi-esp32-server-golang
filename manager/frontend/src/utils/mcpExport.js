// MCP 导出端点（对外平台消费）的纯前端推导
//
// 背景：主服务的工具导出端点 /mcp_export/{agentId}/sse 与 /mcp 共用同一套 agent JWT
// 签名体系（internal/app/server/websocket/mcp_export.go 的 parseExportToken 复用
// parseMCPToken），因此控制台不需要新增后端接口——拿 /mcp-endpoint 已返回的
// `https://host/mcp?token=T` 换成同 host 的 `/mcp_export/{agentId}/sse?token=T` 即可。
//
// 小智官方 / 其他标准 MCP 客户端填入该 SSE 地址后，tools/list 聚合到的正是本 agent
// 的设备侧工具（游戏经 /mcp 端点注册的 npc_* 工具）。
//
// 注意：agentId 必须在路径里，而 /mcp 的 URL 本身不含 agentId，所以调用方要显式传入。

const EMPTY_EXPORT_DATA = {
  ready: false,
  url: '',
  token: '',
  baseURL: '',
  reason: ''
}

/**
 * 由 MCP 接入点 URL 推导 MCP 导出端点（SSE）。
 *
 * @param {string} mcpEndpointUrl 形如 https://wx.originagent.cn/mcp?token=xxx
 * @param {string|number} agentId 智能体 ID（写进导出端点路径）
 * @returns {{ready: boolean, url: string, token: string, baseURL: string, reason: string}}
 */
export function buildMcpExportEndpoint(mcpEndpointUrl, agentId) {
  const trimmedEndpoint = String(mcpEndpointUrl || '').trim()
  const trimmedAgentId = String(agentId == null ? '' : agentId).trim()

  if (!trimmedEndpoint) {
    return { ...EMPTY_EXPORT_DATA, reason: 'missing_endpoint' }
  }
  if (!trimmedAgentId) {
    return { ...EMPTY_EXPORT_DATA, reason: 'missing_agent_id' }
  }

  try {
    const parsed = new URL(trimmedEndpoint)
    const token = String(parsed.searchParams.get('token') || '').trim()
    if (!token) {
      return { ...EMPTY_EXPORT_DATA, reason: 'missing_token' }
    }
    if (!parsed.host) {
      return { ...EMPTY_EXPORT_DATA, reason: 'missing_host' }
    }

    const baseURL = `${parsed.protocol}//${parsed.host}`
    const exportPath = `/mcp_export/${encodeURIComponent(trimmedAgentId)}/sse`
    const url = `${baseURL}${exportPath}?token=${encodeURIComponent(token)}`

    return {
      ready: true,
      url,
      token,
      baseURL,
      reason: ''
    }
  } catch {
    return { ...EMPTY_EXPORT_DATA, reason: 'invalid_endpoint' }
  }
}

/** 推导失败时给用户看的原因文案（不含任何凭据） */
export function describeMcpExportUnavailable(reason) {
  switch (String(reason || '')) {
    case 'missing_agent_id':
      return '请先保存智能体以获取导出端点。'
    case 'missing_token':
      return '接入点 URL 中缺少 token，请刷新后重试。'
    case 'missing_host':
      return '接入点 URL 缺少 host，请检查 OTA 外网地址配置。'
    case 'invalid_endpoint':
      return '接入点 URL 无法解析，请检查 OTA 外网地址配置。'
    case 'missing_endpoint':
    default:
      return '暂无接入点，请先保存智能体并刷新。'
  }
}
