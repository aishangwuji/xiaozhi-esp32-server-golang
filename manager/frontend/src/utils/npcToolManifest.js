// NPC 工具清单（npc_tool_manifest）表单边界校验与载荷组装
//
// 与 manager/backend/controllers/npc_tool_manifest.go 的 validateNpcToolManifestInput 严格对齐：
// 前端先拦一遍，避免用户填错后才吃 400。长度上限必须与后端常量保持一致，
// 后端仍是最终裁决方（前端校验只是体验优化，不是安全边界）。
//
// 语义提醒（决定 UI 文案）：本表只覆盖/摘除设备侧已上报的 MCP 工具，不能新增工具。

export const NPC_TOOL_LIMITS = Object.freeze({
  agentId: 64,
  toolName: 128,
  description: 4000,
  inputSchema: 8192,
  remark: 255
})

/** 全局默认清单的 agent_id 取值（空串） */
export const GLOBAL_AGENT_ID = ''

/**
 * 空串表示"不覆盖"，为 null/undefined 时归一为空串。
 * 与后端 strings.TrimSpace 一致：首尾空白不参与存储。
 */
export function normalizeText(value) {
  return String(value == null ? '' : value).trim()
}

/**
 * input_schema 必须是 JSON 对象文本（数组/标量一律拒绝），与后端边界校验一致。
 *
 * @returns {{ok: true, value: string} | {ok: false, error: string}}
 */
export function validateInputSchema(rawSchema) {
  const schema = normalizeText(rawSchema)
  if (schema === '') {
    return { ok: true, value: '' }
  }
  if (schema.length > NPC_TOOL_LIMITS.inputSchema) {
    return { ok: false, error: `input_schema 过长（上限 ${NPC_TOOL_LIMITS.inputSchema} 字符）` }
  }
  let parsed
  try {
    parsed = JSON.parse(schema)
  } catch {
    return { ok: false, error: 'input_schema 不是合法的 JSON' }
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { ok: false, error: 'input_schema 必须是 JSON 对象（不能是数组或标量）' }
  }
  return { ok: true, value: schema }
}

/**
 * 校验整张表单并返回可直接 POST 的载荷。
 *
 * @param {{agentId?: string, toolName?: string, description?: string, inputSchema?: string, enabled?: boolean, remark?: string}} form
 * @returns {{ok: true, payload: object} | {ok: false, error: string, field: string}}
 */
export function buildManifestPayload(form = {}) {
  const agentId = normalizeText(form.agentId)
  const toolName = normalizeText(form.toolName)
  const description = normalizeText(form.description)
  const remark = normalizeText(form.remark)
  const enabled = form.enabled !== false

  if (agentId.length > NPC_TOOL_LIMITS.agentId) {
    return { ok: false, field: 'agentId', error: `智能体 ID 过长（上限 ${NPC_TOOL_LIMITS.agentId} 字符）` }
  }
  if (toolName === '') {
    return { ok: false, field: 'toolName', error: '工具名不能为空' }
  }
  if (!/^[A-Za-z0-9_.:-]+$/.test(toolName)) {
    return { ok: false, field: 'toolName', error: '工具名只能包含字母、数字、下划线、点、冒号与连字符' }
  }
  if (toolName.length > NPC_TOOL_LIMITS.toolName) {
    return { ok: false, field: 'toolName', error: `工具名过长（上限 ${NPC_TOOL_LIMITS.toolName} 字符）` }
  }
  if (description.length > NPC_TOOL_LIMITS.description) {
    return { ok: false, field: 'description', error: `描述过长（上限 ${NPC_TOOL_LIMITS.description} 字符）` }
  }
  if (remark.length > NPC_TOOL_LIMITS.remark) {
    return { ok: false, field: 'remark', error: `备注过长（上限 ${NPC_TOOL_LIMITS.remark} 字符）` }
  }

  const schemaResult = validateInputSchema(form.inputSchema)
  if (!schemaResult.ok) {
    return { ok: false, field: 'inputSchema', error: schemaResult.error }
  }

  return {
    ok: true,
    payload: {
      agent_id: agentId,
      tool_name: toolName,
      description,
      input_schema: schemaResult.value,
      enabled,
      remark
    }
  }
}

/** 作用域文案：全局默认清单 vs 某智能体专属条目 */
export function describeManifestScope(agentId, agentLabel) {
  const id = normalizeText(agentId)
  if (id === GLOBAL_AGENT_ID) return '全局默认'
  return agentLabel ? `智能体 ${agentLabel}` : `智能体 ${id}`
}

/**
 * 摘要：这条覆盖到底改了什么？用于列表单元格，避免运营点开才知道。
 */
export function describeManifestEffect(row = {}) {
  const effects = []
  if (row.enabled === false) {
    effects.push('摘除工具')
  } else {
    if (normalizeText(row.description) !== '') effects.push('覆盖描述')
    if (normalizeText(row.input_schema) !== '') effects.push('覆盖入参 schema')
    if (effects.length === 0) effects.push('仅登记（不覆盖任何字段）')
  }
  return effects.join(' + ')
}
