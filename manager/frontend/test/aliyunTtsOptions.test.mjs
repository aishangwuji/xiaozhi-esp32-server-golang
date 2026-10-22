import assert from 'node:assert/strict'
import test from 'node:test'

import { reconcileAliyunVoice, serializeAliyunTtsConfig } from '../src/views/admin/forms/aliyunTtsOptions.js'

test('Qwen-TTS saved format follows the actual HTTP response', () => {
  assert.equal(serializeAliyunTtsConfig({ model: 'qwen3-tts-flash', stream: true, format: 'ogg_opus' }).format, 'pcm')
  assert.equal(serializeAliyunTtsConfig({ model: 'qwen3-tts-flash', stream: false, format: 'ogg_opus' }).format, 'wav')
  assert.equal(serializeAliyunTtsConfig({ model: 'qwen-audio-3.0-tts-flash', stream: true, format: 'ogg_opus' }).format, 'ogg_opus')
})

test('model switch replaces the default voice without discarding custom voices', () => {
  const next = [{ value: 'qwen-audio-3.0-tts-flash-longcanzhuyue' }]
  assert.equal(reconcileAliyunVoice([], next, 'Cherry'), next[0].value)
  assert.equal(reconcileAliyunVoice([], next, 'custom-clone'), 'custom-clone')
})
