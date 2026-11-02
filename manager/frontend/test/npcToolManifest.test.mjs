import assert from 'node:assert/strict'
import test from 'node:test'

import {
  NPC_TOOL_LIMITS,
  buildManifestPayload,
  describeManifestEffect,
  describeManifestScope,
  validateInputSchema
} from '../src/utils/npcToolManifest.js'

test('合法表单产出与后端 npcToolManifestRequest 对齐的载荷', () => {
  const result = buildManifestPayload({
    agentId: ' 7 ',
    toolName: ' npc_trade ',
    description: '  与该NPC买卖物品  ',
    inputSchema: '  {"type":"object"}  ',
    enabled: false,
    remark: ' 运营调整 '
  })
  assert.equal(result.ok, true)
  assert.deepEqual(result.payload, {
    agent_id: '7',
    tool_name: 'npc_trade',
    description: '与该NPC买卖物品',
    input_schema: '{"type":"object"}',
    enabled: false,
    remark: '运营调整'
  })
})

test('enabled 缺省为 true（后端指针未传时的默认语义）', () => {
  const result = buildManifestPayload({ toolName: 'npc_trade' })
  assert.equal(result.ok, true)
  assert.equal(result.payload.enabled, true)
  assert.equal(result.payload.agent_id, '')
})

test('工具名为空拒绝', () => {
  const result = buildManifestPayload({ toolName: '   ' })
  assert.equal(result.ok, false)
  assert.equal(result.field, 'toolName')
})

test('工具名长度超限与后端 128 上限一致', () => {
  const tooLong = 'a'.repeat(NPC_TOOL_LIMITS.toolName + 1)
  assert.equal(buildManifestPayload({ toolName: tooLong }).ok, false)

  const atLimit = 'a'.repeat(NPC_TOOL_LIMITS.toolName)
  assert.equal(buildManifestPayload({ toolName: atLimit }).ok, true)
})

test('描述与备注长度上限', () => {
  assert.equal(
    buildManifestPayload({ toolName: 't', description: 'x'.repeat(NPC_TOOL_LIMITS.description + 1) }).ok,
    false
  )
  assert.equal(
    buildManifestPayload({ toolName: 't', remark: 'x'.repeat(NPC_TOOL_LIMITS.remark + 1) }).ok,
    false
  )
})

test('agent_id 长度上限 64', () => {
  assert.equal(buildManifestPayload({ toolName: 't', agentId: 'a'.repeat(65) }).ok, false)
  assert.equal(buildManifestPayload({ toolName: 't', agentId: 'a'.repeat(64) }).ok, true)
})

test('input_schema 空串表示不覆盖', () => {
  assert.deepEqual(validateInputSchema('   '), { ok: true, value: '' })
})

test('input_schema 必须是 JSON 对象，数组与标量一律拒绝', () => {
  assert.equal(validateInputSchema('[]').ok, false)
  assert.equal(validateInputSchema('"x"').ok, false)
  assert.equal(validateInputSchema('123').ok, false)
  assert.equal(validateInputSchema('null').ok, false)
  assert.equal(validateInputSchema('{bad json').ok, false)
  assert.equal(validateInputSchema('{"type":"object","properties":{}}').ok, true)
})

test('input_schema 超长拒绝（8192）', () => {
  const big = `{"type":"object","properties":{"a":{"description":"${'x'.repeat(8200)}"}}}`
  const result = validateInputSchema(big)
  assert.equal(result.ok, false)
})

test('作用域文案区分全局与智能体专属', () => {
  assert.equal(describeManifestScope('', ''), '全局默认')
  assert.equal(describeManifestScope('7', 'Bob'), '智能体 Bob')
  assert.equal(describeManifestScope('7', ''), '智能体 7')
})

test('效果摘要准确表达覆盖/摘除/仅登记', () => {
  assert.equal(describeManifestEffect({ enabled: false, description: 'x' }), '摘除工具')
  assert.equal(describeManifestEffect({ enabled: true, description: 'x' }), '覆盖描述')
  assert.equal(
    describeManifestEffect({ enabled: true, description: 'x', input_schema: '{"type":"object"}' }),
    '覆盖描述 + 覆盖入参 schema'
  )
  assert.equal(describeManifestEffect({ enabled: true }), '仅登记（不覆盖任何字段）')
})
