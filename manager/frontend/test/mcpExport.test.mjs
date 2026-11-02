import assert from 'node:assert/strict'
import test from 'node:test'

import { buildMcpExportEndpoint, describeMcpExportUnavailable } from '../src/utils/mcpExport.js'

test('由 /mcp 接入点推导 /mcp_export/{agentId}/sse，token 原样透传', () => {
  const endpoint = buildMcpExportEndpoint('https://wx.originagent.cn/mcp?token=abc.def-ghi', 7)
  assert.equal(endpoint.ready, true)
  assert.equal(endpoint.url, 'https://wx.originagent.cn/mcp_export/7/sse?token=abc.def-ghi')
  assert.equal(endpoint.baseURL, 'https://wx.originagent.cn')
  assert.equal(endpoint.reason, '')
})

test('保留非默认端口与 http 协议', () => {
  const endpoint = buildMcpExportEndpoint('http://127.0.0.1:8080/mcp?token=tok', 'bob')
  assert.equal(endpoint.url, 'http://127.0.0.1:8080/mcp_export/bob/sse?token=tok')
})

test('agentId 做 URL 编码，避免路径注入', () => {
  const endpoint = buildMcpExportEndpoint('https://host/mcp?token=tok', 'a/../b')
  assert.equal(endpoint.ready, true)
  assert.equal(endpoint.url, 'https://host/mcp_export/a%2F..%2Fb/sse?token=tok')
})

test('缺接入点 / 缺 agentId / 缺 token 时都不产出 URL', () => {
  assert.deepEqual(
    buildMcpExportEndpoint('', 1).ready,
    false
  )
  assert.equal(buildMcpExportEndpoint('', 1).reason, 'missing_endpoint')
  assert.equal(buildMcpExportEndpoint('https://host/mcp?token=t', '').reason, 'missing_agent_id')
  assert.equal(buildMcpExportEndpoint('https://host/mcp', 3).reason, 'missing_token')
})

test('非法 URL 返回 invalid_endpoint 而不是抛错', () => {
  const endpoint = buildMcpExportEndpoint('not-a-url', 1)
  assert.equal(endpoint.ready, false)
  assert.equal(endpoint.url, '')
  assert.equal(endpoint.reason, 'invalid_endpoint')
})

test('不可用原因都有可读文案', () => {
  for (const reason of ['', 'missing_endpoint', 'missing_agent_id', 'missing_token', 'missing_host', 'invalid_endpoint']) {
    const text = describeMcpExportUnavailable(reason)
    assert.equal(typeof text, 'string')
    assert.ok(text.length > 0)
  }
})
