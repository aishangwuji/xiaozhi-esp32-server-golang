// NPC 常驻 Agent 页面的边界校验与载荷组装
//
// 与 manager/backend/controllers/npc_agent.go 的 validateNpcAgentInput /
// UpdateNpcAgentSettings 严格对齐：前端先拦一遍，避免用户填错后才吃 400。
// 后端仍是最终裁决方（前端校验只是体验优化，不是安全边界）。

export const NPC_AGENT_LIMITS = Object.freeze({
  agentId: 64,
  name: 64,
  persona: 2000,
  remark: 255,
  minIntervalSeconds: 5,
  maxIntervalSeconds: 3600,
  minTickSeconds: 1,
  maxTickSeconds: 60,
  minDecideTimeout: 3,
  maxDecideTimeout: 300,
  minStatusChars: 200,
  maxStatusChars: 8000
})

export function normalizeText(value) {
  return String(value == null ? '' : value).trim()
}

/**
 * 校验并组装 NPC 配置载荷。
 *
 * @param {{id?: number, agentId?: string, name?: string, persona?: string, intervalSeconds?: number, enabled?: boolean, allowedActions?: string[], remark?: string}} form
 * @returns {{ok: true, payload: object} | {ok: false, error: string, field: string}}
 */
export function buildNpcAgentPayload(form = {}) {
  const agentId = normalizeText(form.agentId)
  const name = normalizeText(form.name)
  const persona = normalizeText(form.persona)
  const remark = normalizeText(form.remark)
  const intervalSeconds = Number(form.intervalSeconds ?? 30)
  const allowedActions = Array.isArray(form.allowedActions)
    ? form.allowedActions.map((a) => normalizeText(a)).filter(Boolean)
    : []

  if (!agentId) {
    return { ok: false, field: 'agentId', error: 'agent_id 不能为空（必须与游戏侧 MCP 通道 JWT 里的 agentId 一致）' }
  }
  if (agentId.length > NPC_AGENT_LIMITS.agentId) {
    return { ok: false, field: 'agentId', error: `agent_id 过长（上限 ${NPC_AGENT_LIMITS.agentId} 字符）` }
  }
  if (name.length > NPC_AGENT_LIMITS.name) {
    return { ok: false, field: 'name', error: `名称过长（上限 ${NPC_AGENT_LIMITS.name} 字符）` }
  }
  if (persona.length > NPC_AGENT_LIMITS.persona) {
    return { ok: false, field: 'persona', error: `人格设定过长（上限 ${NPC_AGENT_LIMITS.persona} 字符）` }
  }
  if (remark.length > NPC_AGENT_LIMITS.remark) {
    return { ok: false, field: 'remark', error: `备注过长（上限 ${NPC_AGENT_LIMITS.remark} 字符）` }
  }
  if (
    !Number.isFinite(intervalSeconds) ||
    intervalSeconds < NPC_AGENT_LIMITS.minIntervalSeconds ||
    intervalSeconds > NPC_AGENT_LIMITS.maxIntervalSeconds
  ) {
    return {
      ok: false,
      field: 'intervalSeconds',
      error: `决策周期必须在 ${NPC_AGENT_LIMITS.minIntervalSeconds}~${NPC_AGENT_LIMITS.maxIntervalSeconds} 秒之间`
    }
  }

  return {
    ok: true,
    payload: {
      id: form.id || 0,
      agent_id: agentId,
      name,
      persona,
      interval_seconds: Math.round(intervalSeconds),
      enabled: form.enabled !== false,
      allowed_actions: allowedActions,
      remark
    }
  }
}

/**
 * 校验全局设置载荷。字段为 0/空表示"不改这一项"（与后端语义一致）。
 */
export function buildNpcAgentSettingsPayload(form = {}) {
  const tick = Number(form.tickSeconds ?? 0)
  const interval = Number(form.defaultIntervalSeconds ?? 0)
  const decideTimeout = Number(form.decideTimeoutSeconds ?? 0)
  const statusChars = Number(form.maxStatusChars ?? 0)

  if (tick && (tick < NPC_AGENT_LIMITS.minTickSeconds || tick > NPC_AGENT_LIMITS.maxTickSeconds)) {
    return { ok: false, field: 'tickSeconds', error: `扫描粒度必须在 ${NPC_AGENT_LIMITS.minTickSeconds}~${NPC_AGENT_LIMITS.maxTickSeconds} 秒之间` }
  }
  if (
    interval &&
    (interval < NPC_AGENT_LIMITS.minIntervalSeconds || interval > NPC_AGENT_LIMITS.maxIntervalSeconds)
  ) {
    return { ok: false, field: 'defaultIntervalSeconds', error: `默认周期必须在 ${NPC_AGENT_LIMITS.minIntervalSeconds}~${NPC_AGENT_LIMITS.maxIntervalSeconds} 秒之间` }
  }
  if (
    decideTimeout &&
    (decideTimeout < NPC_AGENT_LIMITS.minDecideTimeout || decideTimeout > NPC_AGENT_LIMITS.maxDecideTimeout)
  ) {
    return { ok: false, field: 'decideTimeoutSeconds', error: `决策超时必须在 ${NPC_AGENT_LIMITS.minDecideTimeout}~${NPC_AGENT_LIMITS.maxDecideTimeout} 秒之间` }
  }
  if (statusChars && (statusChars < NPC_AGENT_LIMITS.minStatusChars || statusChars > NPC_AGENT_LIMITS.maxStatusChars)) {
    return { ok: false, field: 'maxStatusChars', error: `状态截断长度必须在 ${NPC_AGENT_LIMITS.minStatusChars}~${NPC_AGENT_LIMITS.maxStatusChars} 之间` }
  }

  return {
    ok: true,
    payload: {
      enabled: !!form.enabled,
      tick_seconds: tick,
      default_interval_seconds: interval,
      decide_timeout_seconds: decideTimeout,
      max_status_chars: statusChars
    }
  }
}

/**
 * 把运行快照按 agent id 建索引，供表格合并（主服务离线时返回空表）。
 */
export function indexRuntimeAgents(runtime) {
  const map = {}
  const list = runtime && Array.isArray(runtime.agents) ? runtime.agents : []
  list.forEach((snap) => {
    if (snap && snap.id) {
      map[snap.id] = snap
    }
  })
  return map
}

/**
 * 一句话解释"为什么 NPC 没在动"。优先级：主服务不可达 > 调度未开启 > 装配报错 > 单 agent 离线。
 */
export function describeRuntime(runtime, settingsManaged) {
  if (!runtime) {
    return { type: 'info', text: '还没有拉到运行状态。' }
  }
  if (runtime.available === false) {
    return { type: 'warning', text: `主服务状态接口不可用：${runtime.error || '未返回原因'}（内嵌部署时通常是主服务未启动或端口不通）` }
  }
  if (runtime.error) {
    return { type: 'error', text: `调度器装配失败：${runtime.error}` }
  }
  if (!runtime.enabled) {
    return {
      type: 'info',
      text: settingsManaged
        ? '调度总开关关闭：NPC 不会自主行动，只会在语音对话中经 npc_act 被驱动。'
        : '当前生效的是 main_config.yaml（调度未开启）。在本页保存一次设置后，将由控制台接管配置。'
    }
  }
  const offline = (runtime.agents || []).filter((a) => !a.online).length
  if (offline > 0) {
    return { type: 'warning', text: `调度已开启，但有 ${offline} 个 NPC 的通道离线（游戏端未连接或 agentId 不匹配）。` }
  }
  return { type: 'success', text: `调度运行中：${runtime.agent_count} 个 NPC 参与，配置来源 ${runtime.config_source || 'yaml'}。` }
}
