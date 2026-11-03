<template>
  <div class="npc-agents-page">
    <el-card class="intro-card" shadow="never">
      <div class="intro-head">
        <div>
          <p class="intro-kicker">NPC Resident Agent</p>
          <h3>NPC 常驻 Agent</h3>
          <p class="intro-text">
            服务端按周期驱动每个 NPC 完成<strong>意图 → 行动 → 结果</strong>：先读 <code>npc_status</code> 拿状态，
            交给决策模型选一个动作，校验后经 <code>npc_act</code> 下发给游戏客户端。
            客户端只提供"身体 + 通用动作脚本"，不做任何自主行为判断。
          </p>
        </div>
        <el-tag type="info" effect="plain" round>生效延迟 ≤ 60s</el-tag>
      </div>

      <el-alert
        v-if="runtimeNotice"
        :type="runtimeNotice.type"
        :closable="false"
        show-icon
        class="runtime-alert"
      >
        <template #title>{{ runtimeNotice.text }}</template>
      </el-alert>

      <div class="runtime-facts">
        <span><strong>配置来源：</strong>{{ runtime.config_source || 'yaml' }}</span>
        <span><strong>参与数量：</strong>{{ runtime.agent_count ?? 0 }}</span>
        <span><strong>决策模型：</strong>{{ llmLabel }}</span>
        <span v-if="runtime.config_applied_at"><strong>上次应用：</strong>{{ formatTime(runtime.config_applied_at) }}</span>
      </div>
      <p class="hint-line">
        决策模型（provider / api_key / model）写在主服务的 <code>npc_agent.llm</code> 配置里，含密钥，
        因此不在本页编辑；本页只管"谁能动、多久动一次、以什么人格动"。
      </p>
    </el-card>

    <el-card class="settings-card" shadow="never">
      <template #header>
        <div class="list-head">
          <div>
            <span class="card-title">调度设置</span>
            <el-tag v-if="!settingsManaged" size="small" type="warning" effect="plain" class="ml-8">
              未接管：当前生效的是 main_config.yaml
            </el-tag>
          </div>
          <div class="list-actions">
            <el-button type="primary" :loading="savingSettings" @click="saveSettings">
              保存设置
            </el-button>
          </div>
        </div>
      </template>

      <el-form label-width="130px" class="settings-form">
        <div class="settings-grid">
          <el-form-item label="总开关">
            <el-switch v-model="settings.enabled" active-text="开启" inactive-text="关闭" />
            <span class="field-hint">关闭后 NPC 不会自主行动，只在语音对话中被驱动</span>
          </el-form-item>
          <el-form-item label="扫描粒度（秒）">
            <el-input-number v-model="settings.tick_seconds" :min="1" :max="60" :step="1" />
            <span class="field-hint">调度器多久检查一次"谁到期了"</span>
          </el-form-item>
          <el-form-item label="默认周期（秒）">
            <el-input-number v-model="settings.default_interval_seconds" :min="5" :max="3600" :step="5" />
            <span class="field-hint">单个 NPC 未单独配周期时用它</span>
          </el-form-item>
          <el-form-item label="决策超时（秒）">
            <el-input-number v-model="settings.decide_timeout_seconds" :min="3" :max="300" :step="1" />
            <span class="field-hint">必须明显小于最短周期，否则会挤占其它 NPC</span>
          </el-form-item>
          <el-form-item label="状态截断（字）">
            <el-input-number v-model="settings.max_status_chars" :min="200" :max="8000" :step="100" />
            <span class="field-hint">状态快照进提示词前的截断长度</span>
          </el-form-item>
        </div>
      </el-form>
    </el-card>

    <el-card class="list-card" shadow="never">
      <template #header>
        <div class="list-head">
          <div>
            <span class="card-title">NPC 列表</span>
            <el-tag v-if="!agentsManaged" size="small" type="info" effect="plain" class="ml-8">
              当前来自 main_config.yaml；在本页新增后由控制台接管
            </el-tag>
          </div>
          <div class="list-actions">
            <el-button :loading="loading" @click="load">
              <el-icon><Refresh /></el-icon>
              刷新
            </el-button>
            <el-button type="primary" @click="openCreate">
              <el-icon><Plus /></el-icon>
              新增 NPC
            </el-button>
          </div>
        </div>
      </template>

      <el-table :data="rows" v-loading="loading" stripe>
        <el-table-column prop="agent_id" label="agent_id" min-width="150">
          <template #default="{ row }">
            <code>{{ row.agent_id }}</code>
            <div v-if="row.runtime" class="sub-cell">
              <el-tag :type="row.runtime.online ? 'success' : 'info'" size="small" effect="plain">
                {{ row.runtime.online ? '通道在线' : '通道离线' }}
              </el-tag>
            </div>
            <div v-else class="sub-cell muted">主服务未上报</div>
          </template>
        </el-table-column>

        <el-table-column prop="name" label="名称" min-width="120" />

        <el-table-column label="周期" width="88">
          <template #default="{ row }">{{ row.interval_seconds }}s</template>
        </el-table-column>

        <el-table-column label="允许动作" min-width="180">
          <template #default="{ row }">
            <span v-if="!row.allowed_actions || row.allowed_actions.length === 0" class="muted">
              默认全集（{{ knownActions.join(' / ') }}）
            </span>
            <el-tag
              v-for="action in row.allowed_actions"
              :key="action"
              size="small"
              effect="plain"
              class="action-tag"
            >
              {{ action }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="启用" width="76">
          <template #default="{ row }">
            <el-switch :model-value="row.enabled" @change="(v) => toggleEnabled(row, v)" />
          </template>
        </el-table-column>

        <el-table-column label="运行状态" min-width="230">
          <template #default="{ row }">
            <template v-if="row.runtime">
              <div class="run-line">
                <strong>{{ row.runtime.last_action || '—' }}</strong>
                <span class="muted">运行 {{ row.runtime.runs }} 次</span>
              </div>
              <div v-if="row.runtime.last_reason" class="sub-cell">理由：{{ row.runtime.last_reason }}</div>
              <div v-if="row.runtime.last_receipt" class="sub-cell muted">回执：{{ row.runtime.last_receipt }}</div>
              <div v-if="row.runtime.last_error" class="sub-cell error-text">错误：{{ row.runtime.last_error }}</div>
              <div v-if="row.runtime.failures > 0 || row.runtime.rejects > 0" class="sub-cell muted">
                失败 {{ row.runtime.failures }} / 拒绝 {{ row.runtime.rejects }} / 跳过 {{ row.runtime.skips }}
              </div>
            </template>
            <span v-else class="muted">—</span>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="130" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && rows.length === 0" description="还没有配置 NPC：新增后按 agent_id 绑定游戏侧通道" />
    </el-card>

    <el-dialog v-model="dialogVisible" :title="form.id ? '编辑 NPC' : '新增 NPC'" width="640px">
      <el-form label-width="110px">
        <el-form-item label="agent_id" required>
          <el-input v-model="form.agentId" placeholder="游戏侧 MCP 通道 JWT 里的 agentId" />
          <div class="field-hint">
            不是游戏里的 npc_id。可在主服务日志里搜 <code>[npcagent]</code> 看当前在线的通道 id。
          </div>
        </el-form-item>
        <el-form-item label="名称">
          <el-input v-model="form.name" maxlength="64" show-word-limit placeholder="铁匠 鲍勃" />
        </el-form-item>
        <el-form-item label="人格设定">
          <el-input
            v-model="form.persona"
            type="textarea"
            :rows="4"
            maxlength="2000"
            show-word-limit
            placeholder="固执直率、重情义的铁匠，白天守着铁匠铺打铁，厌恶流氓在铺子前闹事。"
          />
          <div class="field-hint">这段文字会拼进决策提示词，决定这个 NPC 的行为倾向。</div>
        </el-form-item>
        <el-form-item label="决策周期（秒）">
          <el-input-number v-model="form.intervalSeconds" :min="5" :max="3600" :step="5" />
        </el-form-item>
        <el-form-item label="允许动作">
          <el-select
            v-model="form.allowedActions"
            multiple
            collapse-tags
            collapse-tags-tooltip
            placeholder="留空 = 默认全集"
            style="width: 100%"
          >
            <el-option v-for="action in knownActions" :key="action" :label="action" :value="action" />
          </el-select>
          <div class="field-hint">留空表示允许全部动作；取消勾选某些动作可限制行为边界。</div>
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="form.remark" maxlength="255" show-word-limit placeholder="改动原因，仅人工查阅" />
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submit">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Refresh } from '@element-plus/icons-vue'
import api from '@/utils/api'
import {
  buildNpcAgentPayload,
  buildNpcAgentSettingsPayload,
  describeRuntime,
  indexRuntimeAgents
} from '@/utils/npcAgents'

const loading = ref(false)
const saving = ref(false)
const savingSettings = ref(false)
const dialogVisible = ref(false)

const agents = ref([])
const knownActions = ref(['walk', 'run', 'jump', 'talk', 'attack', 'wait', 'stop'])
const agentsManaged = ref(false)
const settingsManaged = ref(false)
const runtime = ref({})

const settings = reactive({
  enabled: false,
  tick_seconds: 1,
  default_interval_seconds: 30,
  decide_timeout_seconds: 25,
  max_status_chars: 1200
})

const form = reactive({
  id: 0,
  agentId: '',
  name: '',
  persona: '',
  intervalSeconds: 30,
  enabled: true,
  allowedActions: [],
  remark: ''
})

const runtimeMap = computed(() => indexRuntimeAgents(runtime.value))
const rows = computed(() =>
  agents.value.map((row) => ({ ...row, runtime: runtimeMap.value[row.agent_id] || null }))
)
const runtimeNotice = computed(() => describeRuntime(runtime.value, settingsManaged.value))
const llmLabel = computed(() => {
  const llm = runtime.value.llm || {}
  if (!llm.provider) {
    return '未配置（npc_agent.llm.provider）'
  }
  return `${llm.provider}${llm.model_name ? ' / ' + llm.model_name : ''}`
})

function formatTime(value) {
  if (!value) {
    return '—'
  }
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) {
    return String(value)
  }
  return date.toLocaleString()
}

async function load() {
  loading.value = true
  try {
    const response = await api.get('/admin/npc-agents')
    const data = response.data || {}
    agents.value = Array.isArray(data.agents) ? data.agents : []
    knownActions.value = Array.isArray(data.known_actions) && data.known_actions.length
      ? data.known_actions
      : knownActions.value
    agentsManaged.value = !!data.agents_managed
    settingsManaged.value = !!data.settings_managed
    runtime.value = data.runtime || {}

    if (data.settings) {
      settings.enabled = !!data.settings.enabled
      settings.tick_seconds = data.settings.tick_seconds ?? 1
      settings.default_interval_seconds = data.settings.default_interval_seconds ?? 30
      settings.decide_timeout_seconds = data.settings.decide_timeout_seconds ?? 25
      settings.max_status_chars = data.settings.max_status_chars ?? 1200
    }
  } finally {
    loading.value = false
  }
}

async function saveSettings() {
  const built = buildNpcAgentSettingsPayload(settings)
  if (!built.ok) {
    ElMessage.error(built.error)
    return
  }
  savingSettings.value = true
  try {
    await api.put('/admin/npc-agents/settings', built.payload)
    ElMessage.success('已保存，主服务会在 60s 内热更新')
    await load()
  } finally {
    savingSettings.value = false
  }
}

function resetForm() {
  form.id = 0
  form.agentId = ''
  form.name = ''
  form.persona = ''
  form.intervalSeconds = settings.default_interval_seconds || 30
  form.enabled = true
  form.allowedActions = []
  form.remark = ''
}

function openCreate() {
  resetForm()
  dialogVisible.value = true
}

function openEdit(row) {
  form.id = row.id || 0
  form.agentId = row.agent_id || ''
  form.name = row.name || ''
  form.persona = row.persona || ''
  form.intervalSeconds = row.interval_seconds || 30
  form.enabled = row.enabled !== false
  form.allowedActions = Array.isArray(row.allowed_actions) ? [...row.allowed_actions] : []
  form.remark = row.remark || ''
  dialogVisible.value = true
}

async function submit() {
  const built = buildNpcAgentPayload(form)
  if (!built.ok) {
    ElMessage.error(built.error)
    return
  }
  saving.value = true
  try {
    await api.post('/admin/npc-agents', built.payload)
    ElMessage.success('已保存，主服务会在 60s 内热更新')
    dialogVisible.value = false
    await load()
  } finally {
    saving.value = false
  }
}

async function toggleEnabled(row, value) {
  const built = buildNpcAgentPayload({
    id: row.id,
    agentId: row.agent_id,
    name: row.name,
    persona: row.persona,
    intervalSeconds: row.interval_seconds,
    enabled: value,
    allowedActions: row.allowed_actions,
    remark: row.remark
  })
  if (!built.ok) {
    ElMessage.error(built.error)
    return
  }
  try {
    await api.post('/admin/npc-agents', built.payload)
    ElMessage.success(value ? '已启用' : '已停用')
    await load()
  } catch {
    // 拦截器已经提示过，这里只需要把界面拉回真实状态
    await load()
  }
}

async function remove(row) {
  try {
    await ElMessageBox.confirm(
      `删除后该 NPC 会立即停止自主行动（主服务 60s 内生效）。确认删除 ${row.agent_id}？`,
      '删除 NPC 配置',
      { type: 'warning' }
    )
  } catch {
    return
  }
  await api.delete(`/admin/npc-agents/${row.id}`)
  ElMessage.success('已删除')
  await load()
}

onMounted(load)
</script>

<style scoped>
.npc-agents-page {
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.intro-card,
.settings-card,
.list-card {
  border-radius: 14px;
}

.intro-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.intro-kicker {
  margin: 0 0 4px;
  font-size: 12px;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--el-text-color-secondary);
}

.intro-head h3 {
  margin: 0 0 8px;
  font-size: 18px;
}

.intro-text {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  color: var(--el-text-color-regular);
}

.runtime-alert {
  margin-top: 12px;
}

.runtime-facts {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 24px;
  margin-top: 12px;
  font-size: 13px;
  color: var(--el-text-color-regular);
}

.hint-line {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.card-title {
  font-weight: 600;
  font-size: 15px;
}

.ml-8 {
  margin-left: 8px;
}

.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.list-actions {
  display: flex;
  gap: 8px;
}

.settings-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 4px 24px;
}

.field-hint {
  margin-left: 8px;
  font-size: 12px;
  color: var(--el-text-color-secondary);
}

.sub-cell {
  font-size: 12px;
  margin-top: 2px;
}

.run-line {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}

.action-tag {
  margin-right: 4px;
}

.muted {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.error-text {
  color: var(--el-color-danger);
}
</style>
