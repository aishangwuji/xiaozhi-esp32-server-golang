<template>
  <div class="websearch-config">
    <el-form ref="formRef" :model="form" :rules="rules" class="config-form" v-loading="loading">
      <el-card class="config-card" shadow="never">
        <template #header>
          <div class="card-head">
            <div>
              <p class="card-kicker">Web Search</p>
              <h3>联网搜索</h3>
              <p class="card-description">
                基于 DeepSeek 自研搜索引擎，为模型提供 <strong>web_search</strong>（按关键词搜索网页）与
                <strong>web_fetch</strong>（抓取单个网页正文）两个本地工具。
              </p>
            </div>
            <el-tag :type="form.enabled ? 'success' : 'info'" effect="plain" round>
              {{ form.enabled ? (form.deepseek.api_key ? '已启用' : '缺少 API Key') : '已停用' }}
            </el-tag>
          </div>
        </template>

        <div class="field-stack">
          <el-form-item label="启用联网搜索" prop="enabled">
            <div class="switch-field">
              <div>
                <div class="switch-title">允许模型联网查询实时信息</div>
                <div class="field-help">
                  关闭后两个工具都不会注册给模型；保存后服务端自动热更，无需重启。
                </div>
              </div>
              <el-switch v-model="form.enabled" />
            </div>
          </el-form-item>

          <el-form-item label="搜索服务商">
            <el-input model-value="DeepSeek" disabled />
            <div class="field-help">当前仅支持 DeepSeek Search / Fetch 接口（OpenAI 兼容鉴权）。</div>
          </el-form-item>

          <el-form-item label="API Key" prop="deepseek.api_key">
            <el-input
              v-model="form.deepseek.api_key"
              type="password"
              show-password
              clearable
              placeholder="sk-..."
            />
            <div class="field-help">
              必填。未填写时 web_search / web_fetch 不会注册给模型。Key 仅保存在本机控制台数据库。
            </div>
          </el-form-item>

          <el-form-item label="Base URL" prop="deepseek.base_url">
            <el-input v-model="form.deepseek.base_url" placeholder="https://api.deepseek.com" />
          </el-form-item>

          <el-form-item label="请求超时（秒）" prop="deepseek.timeout_seconds">
            <el-input-number
              v-model="form.deepseek.timeout_seconds"
              :min="5"
              :max="120"
              controls-position="right"
              style="width: 100%"
            />
          </el-form-item>

          <div class="probe-row">
            <el-button :loading="probing" @click="handleProbe">测试连通性</el-button>
            <span v-if="probeOk === true" class="probe-result probe-ok">
              连通正常{{ probeDetail ? `（${probeDetail}）` : '' }}
            </span>
            <span v-else-if="probeOk === false" class="probe-result probe-fail">
              {{ probeDetail }}
            </span>
            <span v-else class="probe-result probe-idle">
              用当前填写的 Key 打一次真实搜索请求，验证是否可用。
            </span>
          </div>
        </div>
      </el-card>

      <el-card class="config-card" shadow="never">
        <template #header>
          <div class="card-head">
            <div>
              <p class="card-kicker">Notes</p>
              <h3>使用说明</h3>
            </div>
          </div>
        </template>
        <ul class="notes-list">
          <li>计费：Search / Fetch 按<strong>成功请求次数</strong>计费，5 元 / 千次；无结果、失败、超时不计费。</li>
          <li>限速：Search 100 QPS、Fetch 25 QPS（账号级），超限返回 429，服务端会自动退避重试一次。</li>
          <li>搜索语法：模型可在 query 中直接使用 <code>site:</code>、<code>-site:</code>、<code>"精确短语"</code>、<code>lang:</code>、<code>after:</code>、<code>before:</code>。</li>
          <li>生效方式：保存后服务端收到配置推送会立即重载工具；如未生效，等待一个配置同步周期（5 分钟）或重启服务。</li>
        </ul>
      </el-card>

      <div class="footer-bar">
        <p class="footer-note">保存后写入控制台数据库，并下发给主程序覆盖 main_config.yaml 中的同名配置段。</p>
        <div class="footer-actions">
          <el-button plain :loading="loading" @click="loadConfig">重置为当前配置</el-button>
          <el-button type="primary" :loading="saving" @click="handleSave">保存配置</el-button>
        </div>
      </div>
    </el-form>
  </div>
</template>

<script setup>
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import api from '@/utils/api'

const loading = ref(false)
const saving = ref(false)
const probing = ref(false)
const configId = ref(null)
const formRef = ref()

const probeOk = ref(null)
const probeDetail = ref('')

const createDefaultState = () => ({
  enabled: true,
  deepseek: {
    base_url: 'https://api.deepseek.com',
    api_key: '',
    timeout_seconds: 30
  }
})

const form = reactive(createDefaultState())

const rules = {
  'deepseek.base_url': [
    { required: true, message: '请输入 Base URL', trigger: 'blur' }
  ],
  'deepseek.timeout_seconds': [
    { required: true, message: '请输入超时时间', trigger: 'blur' },
    { type: 'number', min: 5, max: 120, message: '超时时间必须在 5-120 秒之间', trigger: 'blur' }
  ]
}

const resetForm = () => {
  const defaults = createDefaultState()
  form.enabled = defaults.enabled
  form.deepseek.base_url = defaults.deepseek.base_url
  form.deepseek.api_key = defaults.deepseek.api_key
  form.deepseek.timeout_seconds = defaults.deepseek.timeout_seconds
  probeOk.value = null
  probeDetail.value = ''
}

const loadConfig = async () => {
  loading.value = true
  try {
    const response = await api.get('/admin/configs')
    const configs = (response.data?.data || []).filter(item => item.type === 'websearch')

    resetForm()

    if (configs.length > 0) {
      const config = configs.find(item => item.is_default) || configs[0]
      configId.value = config.id
      try {
        const data = JSON.parse(config.json_data || '{}')
        if (typeof data.enabled === 'boolean') {
          form.enabled = data.enabled
        }
        if (data.deepseek) {
          form.deepseek.base_url = data.deepseek.base_url || form.deepseek.base_url
          form.deepseek.api_key = data.deepseek.api_key || ''
          form.deepseek.timeout_seconds = data.deepseek.timeout_seconds || form.deepseek.timeout_seconds
        }
      } catch (error) {
        ElMessage.warning('联网搜索配置格式异常，已回退到默认值')
      }
    } else {
      configId.value = null
    }
  } catch (error) {
    ElMessage.error('加载联网搜索配置失败')
  } finally {
    loading.value = false
  }
}

const handleProbe = async () => {
  if (!form.deepseek.api_key) {
    ElMessage.warning('请先填写 API Key')
    return
  }
  probing.value = true
  probeOk.value = null
  probeDetail.value = ''
  try {
    const response = await api.post('/admin/websearch-configs/test', {
      base_url: form.deepseek.base_url,
      api_key: form.deepseek.api_key
    })
    const data = response.data?.data || {}
    if (data.ok) {
      probeOk.value = true
      probeDetail.value = `返回 ${data.results ?? 0} 条结果`
    } else {
      probeOk.value = false
      probeDetail.value = [data.hint, data.error].filter(Boolean).join('：') || '连通失败'
    }
  } catch (error) {
    probeOk.value = false
    probeDetail.value = error.response?.data?.error || '连通测试失败'
  } finally {
    probing.value = false
  }
}

const handleSave = async () => {
  if (!formRef.value) return

  try {
    await formRef.value.validate()
  } catch {
    return
  }

  if (form.enabled && !form.deepseek.api_key) {
    ElMessage.warning('未填写 API Key，保存后工具仍不会注册给模型')
  }

  saving.value = true
  try {
    const jsonData = JSON.stringify({
      enabled: form.enabled,
      deepseek: {
        base_url: form.deepseek.base_url,
        api_key: form.deepseek.api_key,
        timeout_seconds: form.deepseek.timeout_seconds
      }
    })

    const payload = {
      type: 'websearch',
      name: '联网搜索配置',
      config_id: 'websearch_default',
      provider: 'deepseek',
      is_default: true,
      enabled: form.enabled,
      json_data: jsonData
    }

    if (configId.value) {
      await api.put(`/admin/configs/${configId.value}`, payload)
      ElMessage.success('联网搜索配置已更新')
    } else {
      const response = await api.post('/admin/configs', payload)
      configId.value = response.data?.data?.id || configId.value
      ElMessage.success('联网搜索配置已保存')
    }

    await loadConfig()
  } catch (error) {
    ElMessage.error(error.response?.data?.error || '保存联网搜索配置失败')
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  loadConfig()
})
</script>

<style scoped>
.websearch-config {
  padding: 0 24px 32px;
}

.config-form {
  display: grid;
  gap: 24px;
}

.config-card {
  border: 1px solid rgba(255, 255, 255, 0.88);
  background: rgba(255, 255, 255, 0.88);
  box-shadow: var(--apple-shadow-md);
}

.card-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 16px;
}

.card-kicker {
  display: block;
  margin: 0;
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--apple-text-tertiary);
}

.card-head h3 {
  margin: 8px 0 0;
  font-size: 22px;
  line-height: 1.15;
  letter-spacing: -0.03em;
  color: var(--apple-text);
}

.card-description,
.field-help,
.footer-note,
.notes-list {
  margin: 8px 0 0;
  font-size: 13px;
  line-height: 1.7;
  color: var(--apple-text-secondary);
}

.field-stack {
  display: grid;
  gap: 20px;
}

.switch-field {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px 18px;
  align-items: center;
}

.switch-title {
  font-size: 15px;
  font-weight: 600;
  color: var(--apple-text);
}

.probe-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  padding-top: 8px;
  border-top: 1px solid rgba(229, 229, 234, 0.72);
}

.probe-result {
  font-size: 13px;
  line-height: 1.6;
}

.probe-ok {
  color: #15803d;
}

.probe-fail {
  color: #b91c1c;
}

.probe-idle {
  color: var(--apple-text-tertiary);
}

.notes-list {
  margin: 0;
  padding-left: 20px;
  display: grid;
  gap: 8px;
}

.notes-list code {
  padding: 1px 5px;
  border-radius: 4px;
  background: rgba(0, 0, 0, 0.05);
  font-size: 12px;
}

.footer-bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
}

.footer-actions {
  display: flex;
  gap: 12px;
}

@media (max-width: 768px) {
  .websearch-config {
    padding: 0 16px 24px;
  }

  .footer-bar {
    flex-direction: column;
    align-items: stretch;
  }

  .footer-actions {
    justify-content: flex-end;
  }
}
</style>
