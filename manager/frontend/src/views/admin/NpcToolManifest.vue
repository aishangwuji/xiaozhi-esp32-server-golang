<template>
  <div class="npc-tools-page">
    <el-card class="intro-card" shadow="never">
      <div class="intro-head">
        <div>
          <p class="intro-kicker">NPC Tool Manifest</p>
          <h3>NPC 工具目录</h3>
          <p class="intro-text">
            云端工具目录：对<strong>设备侧（游戏客户端）已上报</strong>的 MCP 工具做目录级调整——
            改写模型看到的描述与入参 schema，或用总开关把工具摘除。
            本页<strong>不新增工具</strong>，客户端 <code>can_offer</code> 仍是工具可用性的第一裁决。
          </p>
        </div>
        <el-tag type="info" effect="plain" round>生效延迟 ≤ 30s</el-tag>
      </div>
      <el-alert type="warning" :closable="false" show-icon class="scope-alert">
        <template #title>作用域规则</template>
        <span>
          <code>全局默认</code> 条目对所有智能体生效；同一工具名存在智能体专属条目时，<strong>专属条目优先</strong>。
        </span>
      </el-alert>
    </el-card>

    <el-card class="list-card" shadow="never">
      <template #header>
        <div class="list-head">
          <div class="list-filters">
            <el-select
              v-model="agentFilter"
              placeholder="按作用域筛选"
              clearable
              filterable
              style="width: 260px"
            >
              <el-option label="全部（含全局默认）" :value="FILTER_ALL" />
              <el-option label="仅全局默认" :value="GLOBAL_AGENT_ID" />
              <el-option
                v-for="agent in agents"
                :key="agent.id"
                :label="`${agent.name || `智能体 ${agent.id}`}（ID ${agent.id}）`"
                :value="String(agent.id)"
              />
            </el-select>
            <el-input
              v-model="keyword"
              placeholder="搜索工具名 / 描述"
              clearable
              style="width: 220px"
            />
          </div>
          <div class="list-actions">
            <el-button @click="loadManifests">
              <el-icon><Refresh /></el-icon>
              刷新
            </el-button>
            <el-button type="primary" @click="openCreate">
              <el-icon><Plus /></el-icon>
              新增条目
            </el-button>
          </div>
        </div>
      </template>

      <el-table :data="filteredRows" v-loading="loading" stripe>
        <el-table-column label="作用域" width="180">
          <template #default="{ row }">
            <el-tag :type="row.agent_id ? 'primary' : 'info'" effect="plain" size="small">
              {{ describeManifestScope(row.agent_id, agentNameOf(row.agent_id)) }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="tool_name" label="工具名" min-width="150">
          <template #default="{ row }">
            <code class="tool-name">{{ row.tool_name }}</code>
            <el-tag v-if="!row.enabled" type="danger" size="small" class="inline-tag">已摘除</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="改动效果" width="200">
          <template #default="{ row }">
            <span class="effect-text">{{ describeManifestEffect(row) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="工具描述" min-width="240">
          <template #default="{ row }">
            <span v-if="row.description" class="desc-text">{{ row.description }}</span>
            <span v-else class="muted-text">未覆盖（沿用设备侧上报）</span>
          </template>
        </el-table-column>
        <el-table-column label="备注" min-width="140">
          <template #default="{ row }">
            <span :class="row.remark ? 'desc-text' : 'muted-text'">{{ row.remark || '—' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="更新时间" width="170">
          <template #default="{ row }">
            {{ row.updated_at ? new Date(row.updated_at).toLocaleString() : '—' }}
          </template>
        </el-table-column>
        <el-table-column label="操作" width="240" fixed="right">
          <template #default="{ row }">
            <el-button size="small" @click="openEdit(row)">编辑</el-button>
            <el-button size="small" :type="row.enabled ? 'warning' : 'success'" @click="toggleEnabled(row)">
              {{ row.enabled ? '摘除' : '启用' }}
            </el-button>
            <el-button size="small" type="danger" @click="removeRow(row)">删除</el-button>
          </template>
        </el-table-column>
      </el-table>

      <el-empty
        v-if="!loading && filteredRows.length === 0"
        :description="keyword ? '没有匹配的条目' : '暂无工具清单条目，云端沿用设备侧上报的原始描述'"
      >
        <el-button v-if="!keyword" type="primary" @click="openCreate">新增第一条</el-button>
      </el-empty>
    </el-card>

    <el-dialog
      v-model="showDialog"
      :title="editingRow ? '编辑工具清单条目' : '新增工具清单条目'"
      width="720px"
      :close-on-click-modal="false"
      @closed="resetForm"
    >
      <el-alert v-if="overrideTarget" type="success" :closable="false" show-icon class="override-alert">
        <template #title>将覆盖已有条目：{{ describeManifestScope(overrideTarget.agent_id, agentNameOf(overrideTarget.agent_id)) }} / {{ overrideTarget.tool_name }}</template>
        <span>同一 (作用域, 工具名) 唯一，保存后直接更新该条目，不会产生重复记录。</span>
      </el-alert>

      <el-form ref="formRef" :model="form" :rules="rules" label-width="130px">
        <el-form-item label="作用域" prop="agentId">
          <el-select
            v-model="form.agentId"
            filterable
            allow-create
            default-first-option
            placeholder="留空或选「全局默认」"
            style="width: 100%"
            @change="invalidateToolNames"
          >
            <el-option label="全局默认（所有智能体）" :value="GLOBAL_AGENT_ID" />
            <el-option
              v-for="agent in agents"
              :key="agent.id"
              :label="`${agent.name || `智能体 ${agent.id}`}（ID ${agent.id}）`"
              :value="String(agent.id)"
            />
          </el-select>
          <div class="field-help">
            全局默认对所有智能体生效；智能体专属条目会覆盖同名的全局条目。允许直接输入未列出的 ID。
          </div>
        </el-form-item>

        <el-form-item label="工具名" prop="toolName">
          <el-select
            v-model="form.toolName"
            filterable
            allow-create
            default-first-option
            placeholder="从设备侧已上报的工具中选择，或直接输入"
            style="width: 100%"
            :loading="toolNameLoading"
            @visible-change="handleToolNameDropdown"
          >
            <el-option
              v-for="tool in knownToolNames"
              :key="tool.name"
              :label="tool.description ? `${tool.name} — ${tool.description}` : tool.name"
              :value="tool.name"
            />
          </el-select>
          <div class="field-help">
            必须与设备侧上报的工具名<strong>严格一致</strong>（如 <code>npc_trade</code>），否则覆盖不会命中。
            <template v-if="toolNameHint">{{ toolNameHint }}</template>
          </div>
        </el-form-item>

        <el-form-item label="启用">
          <div class="switch-field">
            <div>
              <div class="switch-title">{{ form.enabled ? '保留工具（可覆盖描述/schema）' : '从模型工具列表中摘除' }}</div>
              <div class="field-help">
                关闭后该工具仍可由业务层直接调用，但不会出现在 LLM 看到的目录里。这是云端总开关。
              </div>
            </div>
            <el-switch v-model="form.enabled" />
          </div>
        </el-form-item>

        <el-form-item label="工具描述">
          <el-input
            v-model="form.description"
            type="textarea"
            :rows="4"
            maxlength="4000"
            show-word-limit
            :disabled="!form.enabled"
            placeholder="描述模型应在何时调用该工具。留空表示不覆盖设备侧上报的原始描述。"
          />
        </el-form-item>

        <el-form-item label="入参 Schema">
          <el-input
            v-model="form.inputSchema"
            type="textarea"
            :rows="8"
            maxlength="8192"
            show-word-limit
            :disabled="!form.enabled"
            placeholder='openapi3 JSON 对象文本，例如：{"type":"object","properties":{"item_id":{"type":"string"}}}'
            class="mono-input"
          />
          <div class="field-help">
            留空表示不覆盖。必须是<strong>JSON 对象</strong>，数组与标量会被拒绝。
            <el-button link type="primary" size="small" @click="formatSchema">格式化校验</el-button>
          </div>
        </el-form-item>

        <el-form-item label="备注" prop="remark">
          <el-input v-model="form.remark" maxlength="255" show-word-limit placeholder="改动原因，仅供人工查阅" />
        </el-form-item>
      </el-form>

      <template #footer>
        <el-button @click="showDialog = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="submitForm">保存</el-button>
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
  GLOBAL_AGENT_ID,
  NPC_TOOL_LIMITS,
  buildManifestPayload,
  describeManifestEffect,
  describeManifestScope,
  validateInputSchema
} from '@/utils/npcToolManifest'

// agent_id 筛选的"全部"哨兵值：与空串（仅全局）区分开
const FILTER_ALL = '__all__'

const loading = ref(false)
const saving = ref(false)
const rows = ref([])
const agents = ref([])
const agentFilter = ref(FILTER_ALL)
const keyword = ref('')

const showDialog = ref(false)
const editingRow = ref(null)
const formRef = ref(null)

const toolNameLoading = ref(false)
const toolNameTools = ref([])
const toolNamesLoaded = ref(false)

const form = reactive({
  agentId: GLOBAL_AGENT_ID,
  toolName: '',
  description: '',
  inputSchema: '',
  enabled: true,
  remark: ''
})

const rules = {
  toolName: [{ required: true, message: '请输入或选择工具名', trigger: 'change' }]
}

const agentNameOf = (agentId) => {
  if (!agentId) return ''
  const found = agents.value.find((agent) => String(agent.id) === String(agentId))
  return found ? found.name || '' : ''
}

// 作用域筛选放在前端：后端 GET /api/admin/npc-tools 的 agent_id 过滤是等值匹配，
// 空串等于"不过滤"，无法表达"仅全局默认"。一次拉全量再本地过滤语义更准确，
// 且条目量级很小（工具名 × 智能体数），不存在性能顾虑。
const filteredRows = computed(() => {
  const scope = agentFilter.value
  let list = rows.value
  if (scope === GLOBAL_AGENT_ID) {
    list = list.filter((row) => String(row.agent_id || '') === '')
  } else if (scope !== FILTER_ALL && scope != null) {
    list = list.filter((row) => String(row.agent_id || '') === String(scope))
  }

  const kw = keyword.value.trim().toLowerCase()
  if (!kw) return list
  return list.filter((row) => {
    const name = String(row.tool_name || '').toLowerCase()
    const desc = String(row.description || '').toLowerCase()
    const remark = String(row.remark || '').toLowerCase()
    return name.includes(kw) || desc.includes(kw) || remark.includes(kw)
  })
})

const knownToolNames = computed(() => toolNameTools.value)

const toolNameHint = computed(() => {
  if (!toolNamesLoaded.value) return ''
  if (toolNameTools.value.length === 0) {
    return '当前未能从设备侧读取工具列表（设备可能未上线），可直接手工输入工具名。'
  }
  return `已从设备侧读取 ${toolNameTools.value.length} 个工具。`
})

// 保存时会被后端 upsert 覆盖的既有条目（同 agent_id + tool_name）
const overrideTarget = computed(() => {
  const agentID = String(form.agentId || '').trim()
  const toolName = String(form.toolName || '').trim()
  if (!toolName) return null
  return (
    rows.value.find(
      (row) => String(row.agent_id || '') === agentID && String(row.tool_name || '') === toolName
    ) || null
  )
})

const loadAgents = async () => {
  try {
    const response = await api.get('/admin/agents', { silentError: true })
    agents.value = response.data?.data || []
  } catch {
    // 智能体列表仅用于筛选与下拉提示，失败不阻断主流程
    agents.value = []
  }
}

const loadManifests = async () => {
  loading.value = true
  try {
    // 始终拉全量：作用域筛选在前端做（见 filteredRows 注释）
    const response = await api.get('/admin/npc-tools')
    rows.value = response.data?.data?.items || []
  } catch (error) {
    rows.value = []
    ElMessage.error(error.response?.data?.error || '加载 NPC 工具清单失败')
  } finally {
    loading.value = false
  }
}

const openCreate = () => {
  editingRow.value = null
  resetForm()
  // 若当前筛选到某个智能体，新增时默认沿用它，减少手填
  if (agentFilter.value !== FILTER_ALL && agentFilter.value != null) {
    form.agentId = agentFilter.value
  }
  showDialog.value = true
}

const openEdit = (row) => {
  editingRow.value = row
  form.agentId = String(row.agent_id || '')
  form.toolName = String(row.tool_name || '')
  form.description = String(row.description || '')
  form.inputSchema = String(row.input_schema || '')
  form.enabled = row.enabled !== false
  form.remark = String(row.remark || '')
  invalidateToolNames()
  showDialog.value = true
}

const resetForm = () => {
  form.agentId = GLOBAL_AGENT_ID
  form.toolName = ''
  form.description = ''
  form.inputSchema = ''
  form.enabled = true
  form.remark = ''
  invalidateToolNames()
  formRef.value?.clearValidate()
}

// 切换作用域后，原先缓存的工具名单属于上一个智能体，必须作废
const invalidateToolNames = () => {
  toolNamesLoaded.value = false
  toolNameTools.value = []
}

// 设备侧工具名单：管理员作用域下按智能体读取（与诊断面板同一接口），
// 用于下拉提示"这个工具名现在确实存在"，避免手打错字导致覆盖不命中。
const handleToolNameDropdown = async (visible) => {
  if (!visible || toolNamesLoaded.value || toolNameLoading.value) return
  const agentID = String(form.agentId || '').trim()
  if (!agentID) {
    toolNamesLoaded.value = true
    toolNameTools.value = []
    return
  }
  toolNameLoading.value = true
  try {
    const response = await api.get(`/admin/agents/${agentID}/mcp-tools`, { silentError: true })
    toolNameTools.value = response.data?.data?.tools || []
  } catch {
    toolNameTools.value = []
  } finally {
    toolNameLoading.value = false
    toolNamesLoaded.value = true
  }
}

const formatSchema = () => {
  const result = validateInputSchema(form.inputSchema)
  if (!result.ok) {
    ElMessage.error(result.error)
    return
  }
  if (result.value === '') {
    ElMessage.info('当前为空，保存后表示不覆盖入参 schema')
    return
  }
  form.inputSchema = JSON.stringify(JSON.parse(result.value), null, 2)
  ElMessage.success('入参 schema 是合法 JSON 对象')
}

const submitForm = async () => {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return

  const built = buildManifestPayload(form)
  if (!built.ok) {
    ElMessage.error(built.error)
    return
  }

  saving.value = true
  try {
    await api.post('/admin/npc-tools', built.payload)
    ElMessage.success(editingRow.value ? '条目已更新' : '条目已保存')
    showDialog.value = false
    await loadManifests()
  } catch (error) {
    ElMessage.error(error.response?.data?.error || '保存条目失败')
  } finally {
    saving.value = false
  }
}

const toggleEnabled = async (row) => {
  const nextEnabled = row.enabled === false
  const actionText = nextEnabled ? '启用' : '摘除'
  try {
    await ElMessageBox.confirm(
      nextEnabled
        ? `确认让工具 "${row.tool_name}" 重新出现在模型工具目录中？`
        : `确认把工具 "${row.tool_name}" 从模型工具目录中摘除？业务层直调不受影响。`,
      `确认${actionText}`,
      { confirmButtonText: '确定', cancelButtonText: '取消', type: 'warning' }
    )
    await api.post('/admin/npc-tools', {
      agent_id: String(row.agent_id || ''),
      tool_name: row.tool_name,
      description: row.description || '',
      input_schema: row.input_schema || '',
      enabled: nextEnabled,
      remark: row.remark || ''
    })
    ElMessage.success(`已${actionText}`)
    await loadManifests()
  } catch (error) {
    if (error !== 'cancel') {
      ElMessage.error(error.response?.data?.error || `${actionText}失败`)
    }
  }
}

const removeRow = async (row) => {
  try {
    await ElMessageBox.confirm(
      `删除后该工具将回到设备侧上报的原始描述（或重新出现在目录中）。确定删除 "${
        describeManifestScope(row.agent_id, agentNameOf(row.agent_id))
      } / ${row.tool_name}" ？`,
      '确认删除',
      { confirmButtonText: '确定', cancelButtonText: '取消', type: 'warning' }
    )
    await api.delete(`/admin/npc-tools/${row.id}`)
    ElMessage.success('条目已删除')
    await loadManifests()
  } catch (error) {
    if (error !== 'cancel') {
      ElMessage.error(error.response?.data?.error || '删除条目失败')
    }
  }
}

onMounted(async () => {
  await Promise.all([loadAgents(), loadManifests()])
})
</script>

<style scoped>
.npc-tools-page {
  padding: 20px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.intro-card,
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
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--el-color-primary);
}

.intro-head h3 {
  margin: 0 0 8px;
  font-size: 18px;
}

.intro-text {
  margin: 0;
  max-width: 760px;
  line-height: 1.7;
  color: var(--el-text-color-regular);
}

.scope-alert,
.override-alert {
  margin-top: 12px;
}

.list-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.list-filters,
.list-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.tool-name {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  background: var(--el-fill-color-light);
  padding: 2px 6px;
  border-radius: 4px;
}

.inline-tag {
  margin-left: 8px;
}

.effect-text {
  font-size: 13px;
  color: var(--el-text-color-regular);
}

.desc-text {
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
  line-height: 1.5;
}

.muted-text {
  color: var(--el-text-color-placeholder);
  font-size: 13px;
}

.switch-field {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  width: 100%;
}

.switch-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}

.field-help {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.6;
  color: var(--el-text-color-secondary);
}

.mono-input :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
</style>
