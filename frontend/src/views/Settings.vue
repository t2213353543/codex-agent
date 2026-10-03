<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Refresh, Key, Connection, Cpu, RefreshRight, EditPen, Link, Timer, InfoFilled } from '@element-plus/icons-vue'
import { api } from '../api'
import PageHeader from '../components/PageHeader.vue'
import CopyTextDialog from '../components/CopyTextDialog.vue'
import PowerPointService from '../components/PowerPointService.vue'
import ModelRoutesEditor from '../components/ModelRoutesEditor.vue'
import { managementRotationRequestId, useManagementRead } from '../utils/management'
import { formatDuration } from '../utils/format'

const { loading, error, read, cancel } = useManagementRead()
const settings = ref({ admin_key: '', admin_api: false, version: '', settings_version: null, uptime_seconds: null })
const ready = ref(false)
const activeSection = ref('access')
const sectionSearch = ref('')
const sectionLinks = [
  { id: 'access', label: '密钥与访问', description: '管理凭证与权限', icon: Key, tone: 'amber' },
  { id: 'transport', label: '传输与接口', description: 'WebSocket · SSE · JSON', icon: Connection, tone: 'blue' },
  { id: 'powerpoint', label: '推理服务', description: '推理入口与协议配置', icon: Cpu, tone: 'purple' },
  { id: 'routing', label: '模型映射', description: '固定目标与兜底模型', icon: Cpu, tone: 'blue' },
  { id: 'compatibility', label: '兼容与恢复', description: '客户端参数与令牌刷新', icon: RefreshRight, tone: 'green' },
  { id: 'prompt', label: '内置提示词', description: '固定指令与注入方式', icon: EditPen, tone: 'purple' },
  { id: 'session', label: '会话与缓存', description: '账号绑定与缓存复用', icon: Link, tone: 'blue' },
  { id: 'timeouts', label: '上游超时', description: '响应与空闲等待时间', icon: Timer, tone: 'amber' },
  { id: 'system', label: '系统信息', description: '版本与运行状态', icon: InfoFilled, tone: 'green' },
]
const sectionKeywords = { access: 'admin api key 密钥 权限', transport: 'websocket sse json compact 模型列表', powerpoint: 'bps powerpoint codex cli app server 模式 服务 搜索 入口 部署 托管', compatibility: '401 令牌 刷新 恢复', prompt: '提示词 指令 前置 后置', session: 'sticky ttl 会话 cache 缓存 读取 创建 复用', timeouts: '超时 响应头 流式 压缩', system: '版本 运行时间' }
const matchingSections = computed(() => sectionLinks.filter(section => `${section.label} ${section.description} ${sectionKeywords[section.id]}`.toLowerCase().includes(sectionSearch.value.trim().toLowerCase())))
function showSection(id) {
  activeSection.value = id
  document.getElementById(`settings-${id}`)?.scrollIntoView({ behavior: window.matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth', block: 'start' })
}
const pending = ref('')
const controlsDisabled = computed(() => !ready.value || loading.value || !!pending.value)
const promptDraft = ref('')
const promptByteLimit = 32768
const promptBytes = computed(() => new TextEncoder().encode(promptDraft.value).length)
const promptDirty = computed(() => promptDraft.value !== (settings.value.builtin_prompt || ''))
const modelRoutesDraft = ref('')
const modelRoutesDirty = computed(() => modelRoutesDraft.value !== (settings.value.bps_model_routes || ''))
const modelRoutesValid = computed(() => {
  const targets = settings.value.bps_model_route_targets || []
  const sources = new Set()
  for (const raw of modelRoutesDraft.value.split('\n')) {
    const line = raw.trim()
    if (!line || line.startsWith('#')) continue
    const parts = line.split('=').map(value => value.trim())
    if (parts.length !== 2 || !parts[0] || /[\s\p{Cc}]/u.test(parts[0]) || !targets.includes(parts[1]) || sources.has(parts[0])) return false
    sources.add(parts[0])
  }
  return sources.has('*')
})
const cacheRatioSetting = { field: 'cache_read_ratio_percent', min: 0, max: 100, default: 0 }
const numericSettings = [
  { field: 'sticky_session_ttl_seconds', label: '会话绑定有效期', inputLabel: '会话绑定有效期（秒）', saveLabel: '保存有效期', min: 60, max: 2592000, default: 604800, description: '每次请求会续期，范围 60–2,592,000 秒（1 分钟至 30 天），默认 604,800 秒（7 天）。这是网关会话绑定期限，上游提示缓存有效期由服务商决定。' },
  { field: 'upstream_stream_idle_timeout_seconds', label: '流式读取空闲超时', inputLabel: '流式读取空闲超时（秒）', saveLabel: '保存空闲超时', min: 10, max: 3600, default: 300, description: 'HTTP 流连续未收到上游数据时结束等待；持续收到数据的长对话可以继续。范围 10–3,600 秒，默认 300 秒。' },
  { field: 'upstream_response_header_timeout_seconds', label: '响应头等待超时', inputLabel: '响应头等待超时（秒）', saveLabel: '保存响应头超时', min: 5, max: 600, default: 90, description: 'HTTP Responses 和模型列表等待上游响应头的时间。范围 5–600 秒，默认 90 秒。' },
  { field: 'upstream_compact_timeout_seconds', label: '上下文压缩超时', inputLabel: '上下文压缩超时（秒）', saveLabel: '保存压缩超时', min: 30, max: 3600, default: 1200, description: '上下文压缩请求的响应头等待和整体处理期限。范围 30–3,600 秒，默认 1,200 秒。' },
  cacheRatioSetting,
]
const numericDrafts = ref(Object.fromEntries(numericSettings.map(item => [item.field, item.default])))
const ttlSettings = numericSettings.slice(0, 1)
const timeoutSettings = numericSettings.filter(item => item.field.startsWith('upstream_'))
const numericDirty = item => numericDrafts.value[item.field] !== (settings.value[item.field] ?? item.default)
const numericValid = item => Number.isInteger(numericDrafts.value[item.field]) && numericDrafts.value[item.field] >= item.min && numericDrafts.value[item.field] <= item.max
const draftCount = computed(() => Number(promptDirty.value) + Number(modelRoutesDirty.value) + numericSettings.filter(numericDirty).length)
const draftsValid = computed(() => (!promptDirty.value || promptBytes.value <= promptByteLimit) && (!modelRoutesDirty.value || modelRoutesValid.value) && numericSettings.filter(numericDirty).every(numericValid))
function firstDraftSection() { showSection(modelRoutesDirty.value ? 'routing' : promptDirty.value ? 'prompt' : numericDirty(numericSettings[0]) || numericDirty(cacheRatioSetting) ? 'session' : 'timeouts') }
function discardDrafts() {
  promptDraft.value = settings.value.builtin_prompt || ''
  modelRoutesDraft.value = settings.value.bps_model_routes || ''
  for (const item of numericSettings) numericDrafts.value[item.field] = settings.value[item.field] ?? item.default
}
async function restoreDrafts() {
  try { await ElMessageBox.confirm('恢复为最近一次读取或保存的设置？未保存的模型映射、提示词和数值修改会被清除。', '恢复已保存设置', { confirmButtonText: '恢复', cancelButtonText: '继续编辑', type: 'warning' }) }
  catch { return }
  discardDrafts()
}
async function saveDrafts() {
  if (controlsDisabled.value || !draftCount.value || !draftsValid.value) return
  const patch = Object.fromEntries(numericSettings.filter(numericDirty).map(item => [item.field, numericDrafts.value[item.field]]))
  if (promptDirty.value) patch.builtin_prompt = promptDraft.value
  if (modelRoutesDirty.value) patch.bps_model_routes = modelRoutesDraft.value
  const version = settings.value.settings_version
  pending.value = 'drafts'; error.value = ''; cancel()
  try {
    const result = await api.settingsUpdate({ ...patch, settings_version: version })
    if (result.ok === false || !validVersion(result.settings_version) || result.settings_version <= version) {
      await load(); error.value = '服务端未确认保存结果，已重新读取设置，请核对后重试。'; return
    }
    settings.value = { ...settings.value, ...patch, settings_version: result.settings_version }
    ElMessage.success('未保存的修改已全部保存')
  } catch (cause) {
    if (cause?.status === 409) { await load(); error.value = '设置已被修改，已读取最新设置并保留草稿。请核对后重新保存。' }
    else error.value = cause?.message || '保存失败，草稿已保留'
  } finally { pending.value = '' }
}
function protectWindow(event) { if (draftCount.value || pending.value) { event.preventDefault(); event.returnValue = '' } }
function handlePageRefresh(event) { event.preventDefault(); if (!pending.value) load() }
function reportUnsavedState(event) { if (event.detail) { event.detail.dirty = Boolean(draftCount.value); event.detail.busy = Boolean(pending.value) } }
onBeforeRouteLeave(async to => {
  // Login follows session expiry or the shell's explicit logout confirmation.
  // Neither may leave an unauthenticated settings editor on screen.
  if (to.name === 'login') return true
  if (pending.value) { ElMessage.info('正在保存设置，请稍后再离开'); return false }
  if (!draftCount.value) return true
  try {
    await ElMessageBox.confirm('有未保存的设置，离开后这些修改将被放弃。', '离开设置页面？', { confirmButtonText: '放弃修改并离开', cancelButtonText: '继续编辑', type: 'warning', closeOnClickModal: false, closeOnHashChange: false })
    return true
  } catch { return false }
})
const transportSettings = [
  { field: 'websocket_enabled', label: 'WebSocket', aria: '启用 WebSocket', description: '允许新的 WebSocket 连接。关闭后返回 HTTP 426，支持回退的客户端可改用已开启的 HTTP 模式。' },
  { field: 'prefer_websocket', label: 'WebSocket 优先模式', aria: '启用 WebSocket 优先模式', description: '开启后所有支持的请求将优先使用 WebSocket 连接，而不是 HTTP。客户端仍可显式选择传输方式。' },
  { field: 'bps_websocket_reuse', label: 'WebSocket 连接复用', aria: '启用 WebSocket 连接复用', description: '与官方客户端一致的顺序复用：同一账号的空闲连接被下一个请求继续使用，空闲 4 分钟或完成 8 个请求后轮换。仅在 WebSocket 优先模式开启时生效。' },
  { field: 'http_stream_enabled', label: 'HTTP 流式响应（SSE）', aria: '启用 HTTP 流式响应', description: '允许通过 HTTP 逐步接收回复；关闭后流式请求返回 HTTP 403，不影响 WebSocket。' },
  { field: 'http_json_enabled', label: 'HTTP 非流式响应（JSON）', aria: '启用 HTTP 非流式响应', description: '允许通过 HTTP 一次性获取完整回复；关闭后非流式请求返回 HTTP 403。' },
  { field: 'compact_enabled', label: '上下文压缩', aria: '启用上下文压缩', description: '允许客户端调用 Responses compact 压缩历史上下文；关闭后压缩请求返回 HTTP 403。' },
  { field: 'models_enabled', label: '模型列表', aria: '启用模型列表', description: '允许客户端读取可用模型列表；关闭后模型列表请求返回 HTTP 403。' },
]
const compatibilitySettings = [
  { field: 'compatibility_normalization_enabled', label: '客户端参数兼容', aria: '启用客户端参数兼容', description: '规范化受支持的客户端参数。PowerPoint 所需的协议适配始终启用。' },
  { field: 'automatic_token_refresh_enabled', label: '临期令牌自动刷新', aria: '启用临期令牌自动刷新', description: '自动刷新接近到期的账号令牌。关闭后有效令牌仍可使用，过期令牌会被拒绝；管理端手动刷新仍可用。' },
  { field: 'upstream_401_recovery_enabled', label: '上游 401 恢复', aria: '启用上游 401 恢复', description: '上游明确返回 HTTP 401 时，在同一账号上尝试一次令牌刷新并恢复请求。关闭后直接返回原 401 错误，与临期令牌自动刷新独立控制。' },
  { field: 'rate_limit_cooldown_enabled', label: '限流冷却', aria: '启用限流冷却', description: '默认关闭。开启后，上游明确限流会暂停该账号对应模型，等待冷却结束再使用。关闭后仍按现有规则处理当前请求的换号，后续请求可继续使用该账号。' },
]
const promptModes = [
  { value: 'prepend', label: '前置：放在客户端指令之前' },
  { value: 'append', label: '后置：放在客户端指令之后' },
  { value: 'if_empty', label: '仅在客户端未提供指令时补充' },
]
const cacheScopes = [
  { value: 'client', label: '保留客户端缓存分组' },
  { value: 'api_key', label: '同一 API Key 跨会话复用' },
]
const showKey = ref(false)
const rotateRequestId = managementRotationRequestId
const copying = ref(false)
const copyVisible = ref(false)
const copyText = ref('')
function validVersion(value) { return Number.isSafeInteger(value) && value > 0 }
function newRotationId() {
  if (typeof globalThis.crypto?.randomUUID === 'function') return globalThis.crypto.randomUUID()
  if (typeof globalThis.crypto?.getRandomValues !== 'function') throw new Error('浏览器无法生成安全操作 ID，请更换浏览器后重试')
  const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 0x0f) | 0x40
  bytes[8] = (bytes[8] & 0x3f) | 0x80
  const hex = Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
async function load() {
  const keepPrompt = validVersion(settings.value.settings_version) && promptDirty.value
  const keepRoutes = validVersion(settings.value.settings_version) && modelRoutesDirty.value
  const keepNumbers = new Set(validVersion(settings.value.settings_version) ? numericSettings.filter(numericDirty).map(item => item.field) : [])
  ready.value = false
  await read((signal) => api.settings({ signal }), (data) => {
    if (!validVersion(data.settings_version)) throw new Error('服务端未返回有效设置版本，请刷新后重试')
    settings.value = { ...data, admin_key: typeof data.admin_key === 'string' ? data.admin_key : '' }
    if (!keepPrompt) promptDraft.value = typeof data.builtin_prompt === 'string' ? data.builtin_prompt : ''
    if (!keepRoutes) modelRoutesDraft.value = typeof data.bps_model_routes === 'string' ? data.bps_model_routes : ''
    for (const item of numericSettings) {
      if (!keepNumbers.has(item.field)) numericDrafts.value[item.field] = data[item.field] ?? item.default
    }
    ready.value = true
    showKey.value = false
  })
  return ready.value
}
async function saveSetting(field, value) {
  if (controlsDisabled.value) return
  if (field === 'builtin_prompt' && promptBytes.value > promptByteLimit) return
  if (field === 'bps_model_routes' && !modelRoutesValid.value) return
  const numeric = numericSettings.find(item => item.field === field)
  if (numeric && !numericValid(numeric)) return
  if (field === 'builtin_prompt_mode' && !promptModes.some(mode => mode.value === value)) return
  if (field === 'bps_prompt_cache_scope' && !cacheScopes.some(scope => scope.value === value)) return
  cancel()
  pending.value = field
  error.value = ''
  try {
    const result = await api.settingsUpdate({ [field]: value, settings_version: settings.value.settings_version })
    if (result.ok === false || !validVersion(result.settings_version) || result.settings_version <= settings.value.settings_version) {
      await load()
      error.value = '服务端未确认保存结果，已尝试重新读取设置。请核对后重试。'
      return
    }
    settings.value[field] = value
    settings.value.settings_version = result.settings_version
    if (field === 'inference_mode') await load()
    ElMessage.success('设置已保存')
  } catch (cause) {
    if (cause?.status === 409) {
      const hadDrafts = draftCount.value > 0
      const refreshed = await load()
      error.value = refreshed
        ? `设置已被修改，已读取最新设置。请核对后重新操作。${hadDrafts ? '未保存的设置草稿已保留，请核对后再保存。' : ''}`
        : '设置已被修改，读取最新设置失败。请先刷新，核对后再保存。未保存的编辑已保留。'
    } else error.value = cause?.message || '设置保存失败，请重试'
  } finally { pending.value = '' }
}
function savePromptMode(event) {
  const value = event.target.value
  event.target.value = settings.value.builtin_prompt_mode || 'prepend'
  if (value !== (settings.value.builtin_prompt_mode || 'prepend')) saveSetting('builtin_prompt_mode', value)
}
function saveCacheScope(event) {
  const value = event.target.value
  event.target.value = settings.value.bps_prompt_cache_scope || 'client'
  if (value !== (settings.value.bps_prompt_cache_scope || 'client')) saveSetting('bps_prompt_cache_scope', value)
}
async function copyKey() {
  if (copying.value || !settings.value.admin_key) return
  copying.value = true
  try {
    await navigator.clipboard.writeText(settings.value.admin_key)
    ElMessage.success('admin key 已复制')
  } catch {
    copyText.value = settings.value.admin_key
    copyVisible.value = true
  } finally { copying.value = false }
}
async function regenerate() {
  if (controlsDisabled.value) return
  pending.value = 'regenerate'
  try {
    try {
      await ElMessageBox.confirm(
        rotateRequestId.value ? '上次轮换结果尚未确认，将使用相同操作 ID 重试并取回结果。确定继续？' : '重新生成后旧 admin key 立即失效。新密钥只在当前页面显示，请及时保存。确定继续？',
        '重新生成 admin key', { type: 'warning', confirmButtonText: '重新生成', cancelButtonText: '取消' },
      )
    } catch { return }
    cancel()
    error.value = ''
    if (!rotateRequestId.value) rotateRequestId.value = newRotationId()
    const result = await api.rotateAdminKey(rotateRequestId.value)
    if (typeof result.admin_key !== 'string' || !result.admin_key) {
      error.value = '服务端未返回新密钥，请检查管理会话状态后再操作。'
      return
    }
    rotateRequestId.value = ''
    settings.value.admin_key = result.admin_key
    if (validVersion(result.settings_version)) settings.value.settings_version = result.settings_version
    showKey.value = true
    ElMessage.success('admin key 已重新生成，请及时保存')
  } catch (cause) {
    error.value = cause?.message || '重新生成失败，请检查管理会话状态'
  } finally { pending.value = '' }
}
function formatUptime(seconds) {
  if (seconds == null || !Number.isFinite(Number(seconds)) || Number(seconds) < 0) return '—'
  return formatDuration(Number(seconds) * 1000)
}
onMounted(() => { load(); window.addEventListener('beforeunload', protectWindow); window.addEventListener('page-refresh-request', handlePageRefresh); window.addEventListener('page-unsaved-state', reportUnsavedState) })
onBeforeUnmount(() => { settings.value.admin_key = ''; copyText.value = ''; window.removeEventListener('beforeunload', protectWindow); window.removeEventListener('page-refresh-request', handlePageRefresh); window.removeEventListener('page-unsaved-state', reportUnsavedState) })
</script>

<template>
  <div v-loading="loading" class="settings-page">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button aria-label="刷新系统设置" :disabled="loading || !!pending" :icon="Refresh" @click="load">刷新设置</el-button>
    </PageHeader>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <div class="settings-workspace">
      <aside class="settings-outline"><div class="settings-outline-label">工作空间设置 <span>08</span></div><el-input v-model="sectionSearch" clearable aria-label="查找设置" placeholder="查找设置…" class="settings-search" @keydown.enter.prevent="matchingSections[0] && showSection(matchingSections[0].id)" /><p v-if="!matchingSections.length" class="settings-search-empty">没有匹配分类</p><nav aria-label="设置分类"><button v-for="section in matchingSections" :key="section.id" type="button" :class="{ active: activeSection === section.id }" :aria-current="activeSection === section.id ? 'location' : undefined" @click="showSection(section.id)"><span class="settings-nav-icon" :class="'tone-' + section.tone" aria-hidden="true"><el-icon><component :is="section.icon" /></el-icon></span><span><strong>{{ section.label }}</strong><small>{{ section.description }}</small></span><span class="settings-nav-arrow" aria-hidden="true">›</span></button></nav><div class="settings-outline-foot"><span class="settings-runtime-dot" /><span>{{ ready ? '配置已读取' : loading ? '正在读取配置' : '等待配置就绪' }}</span><small>Codex Gateway {{ settings.version || '' }}</small></div></aside>
    <div class="settings-panel">
      <p class="settings-page-note" role="status"><el-icon aria-hidden="true"><InfoFilled /></el-icon><span>开关与选择项更改后保存；提示词和时间数值需点击保存。刷新会保留未保存的编辑。<span v-if="draftCount" class="settings-draft"> {{ draftCount }} 项未保存</span></span></p>
      <section id="settings-access" class="settings-section" aria-label="密钥管理设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-amber" aria-hidden="true"><el-icon><Key /></el-icon></span><span>密钥管理</span><span class="settings-section-badge tone-amber">访问凭证</span></div>
        <div class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label"><code class="settings-key-label">admin key</code></div>
            <div class="settings-row-desc">用于建立管理会话；已保存的密钥由后端脱敏，新密钥仅在本页面临时显示</div>
          </div>
          <div class="settings-row-control">
            <span class="mono settings-key">{{ settings.admin_key ? (showKey ? settings.admin_key : '••••••••••••••••') : (settings.admin_key_masked || '后端不返回已保存密钥') }}</span>
            <el-button link type="primary" size="small" :disabled="!settings.admin_key" @click="showKey = !showKey">{{ showKey ? '隐藏' : '显示' }}</el-button>
            <el-button link type="primary" size="small" :disabled="!settings.admin_key" :loading="copying" @click="copyKey">复制</el-button>
            <el-button size="small" type="danger" plain :disabled="controlsDisabled" :loading="pending === 'regenerate'" @click="regenerate">{{ rotateRequestId ? '重试轮换结果' : '重新生成' }}</el-button>
          </div>
        </div>
        <div class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label"><code class="settings-key-label">admin api</code>（网关开关）</div>
            <div class="settings-row-desc">开启后 admin key 可直接调用网关；关闭后仅保留管理控制台权限</div>
          </div>
          <div class="settings-row-control">
            <el-switch :model-value="settings.admin_api === true" :disabled="controlsDisabled" :loading="pending === 'admin_api'" aria-label="允许 admin key 调用网关" @change="value => saveSetting('admin_api', value)" />
            <span class="settings-switch-hint">{{ settings.admin_api ? '已开启' : '已关闭' }}</span>
          </div>
        </div>
      </section>

      <section id="settings-transport" class="settings-section" aria-label="网关传输设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-blue" aria-hidden="true"><el-icon><Connection /></el-icon></span><span>网关传输与接口</span><span class="settings-section-badge tone-blue">接口控制</span></div>
        <div v-for="item in transportSettings" :key="item.field" class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label">{{ item.label }}</div>
            <div class="settings-row-desc">{{ item.description }}</div>
          </div>
          <div class="settings-row-control">
            <el-switch :model-value="settings[item.field] === true" :disabled="controlsDisabled" :loading="pending === item.field" :aria-label="item.aria" @change="value => saveSetting(item.field, value)" />
            <span class="settings-switch-hint">{{ settings[item.field] ? '已开启' : '已关闭' }}</span>
          </div>
        </div>
      </section>

      <PowerPointService id="settings-powerpoint" class="settings-section" :inference="settings.inference" :mode="settings.inference_mode" :modes="settings.inference_modes" :disabled="controlsDisabled" :saving="pending === 'inference_mode'" advanced @change-mode="value => saveSetting('inference_mode', value)" />

      <section v-if="settings.bps_model_routes !== undefined" id="settings-routing" class="settings-section" aria-label="固定模型映射设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-blue" aria-hidden="true"><el-icon><Cpu /></el-icon></span><span>固定模型映射</span><span class="settings-section-badge tone-blue">一次确定目标</span></div>
        <div class="settings-routing-content">
          <ModelRoutesEditor
            v-model="modelRoutesDraft"
            :targets="settings.bps_model_route_targets || []"
            :disabled="controlsDisabled"
          />
          <div class="settings-row-control">
            <span v-if="modelRoutesDirty" class="settings-draft">未保存</span>
            <span v-if="!modelRoutesValid" class="settings-row-desc settings-invalid">目标须在已核实名单中；来源不能重复，并须有 * 兜底规则。</span>
            <el-button :disabled="controlsDisabled || !modelRoutesDirty" @click="modelRoutesDraft = settings.bps_model_routes || ''">撤销修改</el-button>
            <el-button type="primary" :disabled="controlsDisabled || !modelRoutesDirty || !modelRoutesValid" :loading="pending === 'bps_model_routes'" @click="saveSetting('bps_model_routes', modelRoutesDraft)">保存模型映射</el-button>
          </div>
        </div>
      </section>

      <section id="settings-compatibility" class="settings-section" aria-label="请求兼容与账号恢复设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-green" aria-hidden="true"><el-icon><RefreshRight /></el-icon></span><span>请求兼容与账号恢复</span><span class="settings-section-badge tone-green">请求策略</span></div>
        <div v-for="item in compatibilitySettings" :key="item.field" class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label">{{ item.label }}</div>
            <div class="settings-row-desc">{{ item.description }}</div>
          </div>
          <div class="settings-row-control">
            <el-switch :model-value="settings[item.field] === true" :disabled="controlsDisabled" :loading="pending === item.field" :aria-label="item.aria" @change="value => saveSetting(item.field, value)" />
            <span class="settings-switch-hint">{{ settings[item.field] ? '已开启' : '已关闭' }}</span>
          </div>
        </div>
      </section>

      <section id="settings-prompt" class="settings-section" aria-label="内置提示词设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-purple" aria-hidden="true"><el-icon><EditPen /></el-icon></span><span>内置提示词</span><span class="settings-section-badge tone-purple">固定指令</span></div>
        <div class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label">自动附加提示词</div>
            <div class="settings-row-desc">启用后对话请求自动带上已保存的固定内容。内容保持稳定有助于复用提示缓存；关闭开关会保留文本。</div>
          </div>
          <div class="settings-row-control">
            <el-switch :model-value="settings.builtin_prompt_enabled === true" :disabled="controlsDisabled" :loading="pending === 'builtin_prompt_enabled'" aria-label="启用内置提示词" @change="value => saveSetting('builtin_prompt_enabled', value)" />
            <span class="settings-switch-hint">{{ settings.builtin_prompt_enabled ? '已开启' : '已关闭' }}</span>
          </div>
        </div>
        <div class="settings-row">
          <div class="settings-row-info">
            <label for="builtin-prompt-mode" class="settings-row-label">提示词注入方式</label>
            <div id="builtin-prompt-mode-help" class="settings-row-desc">选择固定内容与客户端指令的组合方式；开关关闭时也可预选。“仅未提供时补充”适用于指令为空或只有空白的情况，不覆盖已有指令。</div>
          </div>
          <div class="settings-row-control settings-select-control">
            <select id="builtin-prompt-mode" class="settings-select" :value="settings.builtin_prompt_mode || 'prepend'" :disabled="controlsDisabled" :aria-busy="pending === 'builtin_prompt_mode'" aria-label="提示词注入方式" aria-describedby="builtin-prompt-mode-help" @change="savePromptMode">
              <option v-for="mode in promptModes" :key="mode.value" :value="mode.value">{{ mode.label }}</option>
            </select>
            <span v-if="pending === 'builtin_prompt_mode'" class="settings-switch-hint" role="status">保存中</span>
          </div>
        </div>
        <div class="settings-editor">
          <label for="builtin-prompt" class="settings-row-label">提示词内容</label>
          <el-input id="builtin-prompt" v-model="promptDraft" type="textarea" :rows="6" resize="vertical" :disabled="controlsDisabled" aria-label="内置提示词内容" aria-describedby="builtin-prompt-help" :aria-invalid="promptBytes > promptByteLimit" placeholder="输入每次对话都需要附加的固定提示词" />
          <div id="builtin-prompt-help" class="settings-row-desc">编辑后点击保存才会生效。留空表示不附加任何内容，即使开关已开启。</div>
          <div class="settings-editor-footer">
            <span class="settings-row-desc" :class="{ 'settings-invalid': promptBytes > promptByteLimit }" aria-live="polite">{{ promptBytes.toLocaleString() }} / {{ promptByteLimit.toLocaleString() }} 字节（UTF-8）{{ promptBytes > promptByteLimit ? '，内容超出上限' : '' }}</span>
            <div class="settings-row-control">
              <span v-if="promptDirty" class="settings-draft">未保存</span>
              <el-button :disabled="controlsDisabled || !promptDirty" @click="promptDraft = settings.builtin_prompt || ''">撤销编辑</el-button>
              <el-button type="primary" :disabled="controlsDisabled || !promptDirty || promptBytes > promptByteLimit" :loading="pending === 'builtin_prompt'" @click="saveSetting('builtin_prompt', promptDraft)">保存提示词</el-button>
            </div>
          </div>
        </div>
      </section>

      <section id="settings-session" class="settings-section" aria-label="会话与缓存设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-blue" aria-hidden="true"><el-icon><Link /></el-icon></span><span>会话与缓存</span><span class="settings-section-badge tone-blue">复用策略</span></div>
        <div class="settings-row">
          <div class="settings-row-info">
            <div class="settings-row-label">粘性会话</div>
            <div class="settings-row-desc">会话固定使用相同账号，并复用可用代理出口。账号忙时需稍后重试；账号不可用时需修复账号或创建新会话。代理被删除或停用时会重选可用代理。</div>
          </div>
          <div class="settings-row-control">
<span class="settings-section-badge tone-blue" aria-label="会话固定使用同一账号">固定开启</span>
          </div>
        </div>
        <div v-if="settings.inference_mode === 'powerpoint_bps' && settings.bps_prompt_cache_scope !== undefined" class="settings-row">
          <div class="settings-row-info">
            <label for="bps-cache-scope" class="settings-row-label">缓存复用范围</label>
            <div id="bps-cache-scope-help" class="settings-row-desc">跨会话复用可减少相同内容的重复创建。不同 API Key、账号和模型分别缓存，会话与账号绑定各自保留。</div>
          </div>
          <div class="settings-row-control">
            <select id="bps-cache-scope" class="settings-select" :value="settings.bps_prompt_cache_scope || 'client'" :disabled="controlsDisabled || settings.prompt_cache_key_enabled === false" :aria-busy="pending === 'bps_prompt_cache_scope'" aria-label="缓存复用范围" aria-describedby="bps-cache-scope-help" @change="saveCacheScope">
              <option v-for="scope in cacheScopes" :key="scope.value" :value="scope.value">{{ scope.label }}</option>
            </select>
            <span v-if="pending === 'bps_prompt_cache_scope'" class="settings-switch-hint" role="status">保存中</span>
          </div>
        </div>
        <div class="settings-row" data-setting="cache_creation_as_read_enabled">
          <div class="settings-row-info">
            <div class="settings-row-label">缓存创建按读取返回</div>
            <div id="cache-creation-as-read-help" class="settings-row-desc">对外返回的缓存读取量包含创建量，便于下游按读取价格计费；本系统保留原始读写用量。</div>
          </div>
          <div class="settings-row-control">
            <el-switch :model-value="settings.cache_creation_as_read_enabled !== false" :disabled="controlsDisabled" :loading="pending === 'cache_creation_as_read_enabled'" aria-label="缓存创建按读取返回" aria-describedby="cache-creation-as-read-help" @update:model-value="value => saveSetting('cache_creation_as_read_enabled', value)" />
            <span v-if="pending === 'cache_creation_as_read_enabled'" class="settings-switch-hint" role="status">保存中</span>
          </div>
        </div>
        <div class="settings-row settings-number-row" data-setting="cache_read_ratio_enabled">
          <div class="settings-row-info">
            <label for="cache-read-ratio-percent" class="settings-row-label">缓存率控制</label>
            <div id="cache-read-ratio-help" class="settings-row-desc">默认关闭，范围 0–100%。仅调整对外返回的缓存读取量，本系统保留原始用量；按输入 Token × 比例取整。关闭后保留已保存的比例。</div>
          </div>
          <div class="settings-number-control">
            <div class="settings-row-control">
              <el-switch :model-value="settings.cache_read_ratio_enabled === true" :disabled="controlsDisabled" :loading="pending === 'cache_read_ratio_enabled'" aria-label="启用缓存率控制" aria-describedby="cache-read-ratio-help" @update:model-value="value => saveSetting('cache_read_ratio_enabled', value)" />
              <span class="settings-switch-hint" :role="pending === 'cache_read_ratio_enabled' ? 'status' : undefined">{{ pending === 'cache_read_ratio_enabled' ? '保存中' : settings.cache_read_ratio_enabled ? '已开启' : '已关闭' }}</span>
              <el-input id="cache-read-ratio-percent" type="number" :model-value="numericDrafts.cache_read_ratio_percent ?? ''" :min="cacheRatioSetting.min" :max="cacheRatioSetting.max" :step="1" :disabled="controlsDisabled" aria-label="缓存读取比例（%）" aria-describedby="cache-read-ratio-help" :aria-invalid="!numericValid(cacheRatioSetting)" @update:model-value="value => numericDrafts.cache_read_ratio_percent = value === '' ? null : Number(value)" />
              <span class="settings-switch-hint">%</span>
            </div>
            <div class="settings-row-control">
              <span v-if="numericDirty(cacheRatioSetting)" class="settings-draft">未保存</span>
              <el-button :disabled="controlsDisabled || !numericDirty(cacheRatioSetting)" @click="numericDrafts.cache_read_ratio_percent = settings.cache_read_ratio_percent ?? cacheRatioSetting.default">撤销修改</el-button>
              <el-button type="primary" :disabled="controlsDisabled || !numericDirty(cacheRatioSetting) || !numericValid(cacheRatioSetting)" :loading="pending === 'cache_read_ratio_percent'" @click="saveSetting('cache_read_ratio_percent', numericDrafts.cache_read_ratio_percent)">保存比例</el-button>
            </div>
            <span v-if="!numericValid(cacheRatioSetting)" class="settings-row-desc settings-invalid">请输入 0 至 100 之间的整数百分比。</span>
          </div>
        </div>
        <div v-for="item in ttlSettings" :key="item.field" class="settings-row settings-number-row">
          <div class="settings-row-info">
            <label :for="item.field" class="settings-row-label">{{ item.label }}</label>
            <div :id="`${item.field}-help`" class="settings-row-desc">{{ item.description }}</div>
          </div>
          <div class="settings-number-control">
            <div class="settings-row-control">
              <el-input :id="item.field" type="number" :model-value="numericDrafts[item.field] ?? ''" :min="item.min" :max="item.max" :step="1" :disabled="controlsDisabled" :aria-label="item.inputLabel" :aria-describedby="`${item.field}-help`" :aria-invalid="!numericValid(item)" @update:model-value="value => numericDrafts[item.field] = value === '' ? null : Number(value)" />
              <span class="settings-switch-hint">秒</span>
            </div>
            <div class="settings-row-control">
              <span v-if="numericDirty(item)" class="settings-draft">未保存</span>
              <el-button :disabled="controlsDisabled || !numericDirty(item)" @click="numericDrafts[item.field] = settings[item.field] ?? item.default">撤销修改</el-button>
              <el-button type="primary" :disabled="controlsDisabled || !numericDirty(item) || !numericValid(item)" :loading="pending === item.field" @click="saveSetting(item.field, numericDrafts[item.field])">{{ item.saveLabel }}</el-button>
            </div>
            <span v-if="!numericValid(item)" class="settings-row-desc settings-invalid">请输入 {{ item.min.toLocaleString() }} 至 {{ item.max.toLocaleString() }} 之间的整数秒。</span>
          </div>
        </div>
      </section>

      <section id="settings-timeouts" class="settings-section" aria-label="上游超时设置">
        <div class="settings-section-title"><span class="settings-title-icon tone-amber" aria-hidden="true"><el-icon><Timer /></el-icon></span><span>上游超时</span><span class="settings-section-badge tone-amber">等待期限</span></div>
        <div v-for="item in timeoutSettings" :key="item.field" class="settings-row settings-number-row">
          <div class="settings-row-info">
            <label :for="item.field" class="settings-row-label">{{ item.label }}</label>
            <div :id="`${item.field}-help`" class="settings-row-desc">{{ item.description }}</div>
          </div>
          <div class="settings-number-control">
            <div class="settings-row-control">
              <el-input :id="item.field" type="number" :model-value="numericDrafts[item.field] ?? ''" :min="item.min" :max="item.max" :step="1" :disabled="controlsDisabled" :aria-label="item.inputLabel" :aria-describedby="`${item.field}-help`" :aria-invalid="!numericValid(item)" @update:model-value="value => numericDrafts[item.field] = value === '' ? null : Number(value)" />
              <span class="settings-switch-hint">秒</span>
            </div>
            <div class="settings-row-control">
              <span v-if="numericDirty(item)" class="settings-draft">未保存</span>
              <el-button :disabled="controlsDisabled || !numericDirty(item)" @click="numericDrafts[item.field] = settings[item.field] ?? item.default">撤销修改</el-button>
              <el-button type="primary" :disabled="controlsDisabled || !numericDirty(item) || !numericValid(item)" :loading="pending === item.field" @click="saveSetting(item.field, numericDrafts[item.field])">{{ item.saveLabel }}</el-button>
            </div>
            <span v-if="!numericValid(item)" class="settings-row-desc settings-invalid">请输入 {{ item.min.toLocaleString() }} 至 {{ item.max.toLocaleString() }} 之间的整数秒。</span>
          </div>
        </div>
      </section>

      <section id="settings-system" class="settings-section" aria-label="系统信息">
        <div class="settings-section-title"><span class="settings-title-icon tone-green" aria-hidden="true"><el-icon><InfoFilled /></el-icon></span><span>系统信息</span><span class="settings-section-badge tone-green">只读</span></div>
        <div class="settings-row">
          <div class="settings-row-info"><div class="settings-row-label">版本</div><div class="settings-row-desc">后端返回的运行版本</div></div>
          <div class="settings-row-control mono">{{ settings.version || '—' }}</div>
        </div>
        <div class="settings-row">
          <div class="settings-row-info"><div class="settings-row-label">运行时长</div><div class="settings-row-desc">自上次启动以来的持续运行时间</div></div>
          <div class="settings-row-control mono">{{ formatUptime(settings.uptime_seconds) }}</div>
        </div>
      </section>
    </div>
    </div>
    <div v-if="draftCount" class="settings-save-bar" role="region" aria-label="未保存的设置">
      <div><strong>{{ draftCount }} 项修改未保存</strong><span>{{ draftsValid ? '保存后应用到新的请求。' : '请先修正模型映射、提示词长度或数值。' }}</span></div>
      <el-button link @click="firstDraftSection">定位修改</el-button>
      <el-button :disabled="controlsDisabled" @click="restoreDrafts">恢复已保存</el-button>
      <el-button type="primary" :disabled="controlsDisabled || !draftsValid" :loading="pending === 'drafts'" @click="saveDrafts">保存全部修改</el-button>
    </div>
    <CopyTextDialog v-model="copyVisible" :text="copyText" @update:model-value="visible => { if (!visible) copyText = '' }" />
  </div>
</template>

<style scoped>
.settings-page, .settings-panel { min-width: 0; }
.settings-search { margin-bottom: 9px; }.settings-search-empty { color:var(--text-3);font-size:11px;padding:8px; }
.settings-save-bar { position:sticky;bottom:12px;z-index:5;display:flex;align-items:center;gap:10px;flex-wrap:wrap;padding:14px 18px;background:var(--card-bg);border:1px solid var(--primary);border-radius:10px;box-shadow:0 4px 24px #0000000d;margin-top:16px; }
.settings-save-bar>div { flex:1;min-width:180px; }.settings-save-bar strong { display:block;font-size:13px; }.settings-save-bar span { display:block;font-size:11px;color:var(--text-3);margin-top:4px; }.settings-save-bar .el-button { margin-left:0; }
@media(max-width:600px) { .settings-save-bar { bottom:8px;padding:12px;gap:8px; }.settings-save-bar>div { flex-basis:100%; } }
.settings-page :deep(.page-header) { margin-bottom: 16px; }
.settings-workspace { display: grid; grid-template-columns: 184px minmax(0,1fr); align-items: start; gap: 18px; margin-top: 14px; }
.settings-outline { position: sticky; top: 12px; min-width: 0; padding: 12px 8px 13px; border: 1px solid var(--card-border); border-radius: 10px; background: var(--card-bg); }
.settings-outline-label { display: flex; justify-content: space-between; align-items: center; margin: 0 0 10px; padding: 2px 9px; color: var(--text-3); font-size: 10px; }
.settings-outline-label > span { padding: 2px 5px; border-radius: 4px; background: var(--bg); font-size: 8px; font-variant-numeric: tabular-nums; }
.settings-outline nav { display: flex; flex-direction: column; gap: 2px; }
.settings-outline nav button { display: flex; align-items: center; gap: 9px; width: 100%; min-height: 49px; padding: 8px; border: 1px solid transparent; border-radius: 7px; background: transparent; color: var(--text-2); text-align: left; font: inherit; cursor: pointer; }
.settings-outline nav button:hover { background: var(--bg); }
.settings-outline nav button.active { color: var(--primary); background: var(--primary-soft); border-color: color-mix(in srgb,var(--primary) 10%,transparent); }
.settings-outline nav button:focus-visible { outline: 2px solid var(--primary); outline-offset: 2px; }
.settings-nav-icon { display: grid; place-items: center; width: 25px; height: 25px; flex-shrink: 0; border-radius: 6px; font-size: 13px; }
.settings-outline nav button strong { display: block; font-size: 11px; font-weight: 550; }
.settings-outline nav button small { display: block; margin-top: 4px; color: var(--text-3); font-size: 8px; }
.settings-nav-arrow { margin-left: auto; color: var(--primary); opacity: 0; font-size: 17px; }
.settings-outline nav button.active .settings-nav-arrow { opacity: 1; }
.settings-outline-foot { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; margin: 12px 8px 0; padding-top: 12px; border-top: 1px solid var(--card-border); color: var(--text-3); font-size: 9px; }
.settings-runtime-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--text-3); }
.settings-outline-foot small { width: 100%; margin: 2px 0 0 11px; font-size: 8px; overflow-wrap: anywhere; }
.settings-panel { padding: 0; max-width: none; }
.settings-page-note { display: flex; align-items: flex-start; gap: 9px; margin: 0 0 12px; padding: 10px 12px; border: 1px solid color-mix(in srgb,var(--badge-blue,var(--primary)) 14%,var(--card-border)); border-radius: 7px; background: var(--badge-blue-soft,var(--primary-soft)); color: var(--text-2); font-size: 10px; line-height: 1.75; }
.settings-page-note > .el-icon { flex-shrink: 0; margin-top: 3px; color: var(--badge-blue,var(--primary)); font-size: 13px; }
.settings-panel :deep(.settings-section) { min-width: 0; padding: 14px 17px 0; margin-bottom: 14px; border: 1px solid var(--card-border); border-radius: 9px; background: var(--card-bg); scroll-margin-top: 16px; box-shadow: none; }
.settings-panel :deep(.settings-section-title) { display: flex; align-items: center; gap: 8px; min-height: 29px; margin: 0; padding: 0 0 12px; border-bottom: 1px solid var(--card-border); color: var(--text-1); font-size: 12px; font-weight: 600; letter-spacing: -.15px; }
.settings-panel :deep(.settings-section-title)::before { display: none; }
.settings-title-icon { display: grid; place-items: center; width: 26px; height: 26px; border-radius: 7px; font-size: 14px; flex-shrink: 0; }
.settings-section-badge { margin-left: auto; padding: 3px 6px; border-radius: 4px; font-size: 8px; font-weight: 500; letter-spacing: .2px; white-space: nowrap; }
.tone-blue { color: var(--badge-blue,var(--primary)); background: var(--badge-blue-soft,var(--primary-soft)); }
.tone-purple { color: var(--badge-purple,var(--primary)); background: var(--badge-purple-soft,var(--primary-soft)); }
.tone-green { color: var(--badge-green,var(--success)); background: var(--badge-green-soft,var(--primary-soft)); }
.tone-amber { color: var(--badge-amber,var(--warning)); background: var(--badge-amber-soft,var(--primary-soft)); }
.settings-panel :deep(.settings-row) { display: flex; align-items: flex-start; flex-direction: row; flex-wrap: nowrap; gap: 18px; padding: 13px 0; }
.settings-panel :deep(.settings-row + .settings-row) { border-top: 1px solid var(--card-border); }
.settings-panel :deep(.settings-row-info) { flex: 1 1 45%; min-width: 0; }
.settings-panel :deep(.settings-row-label) { color: var(--text-1); font-size: 11px; font-weight: 550; line-height: 1.65; }
.settings-panel :deep(.settings-row-desc) { max-width: none; margin-top: 5px; color: var(--text-3); font-size: 10px; line-height: 1.75; overflow-wrap: anywhere; }
.settings-panel :deep(.settings-row-control) { display: flex; align-items: center; gap: 7px; flex-wrap: wrap; flex-shrink: 1; min-width: 0; }
.settings-panel :deep(.settings-switch-hint) { min-width: 34px; font-size: 9px; color: var(--text-3); }
.settings-panel :deep(.el-switch) { margin-top: -3px; height: 25px; }
.settings-key-label { padding: 2px 5px; border: 1px solid color-mix(in srgb,var(--badge-amber,var(--warning)) 16%,var(--card-border)); border-radius: 4px; color: var(--badge-amber,var(--warning)); background: var(--badge-amber-soft,var(--primary-soft)); font: 500 10px Consolas,monospace; }
.settings-key { display: block; max-width: min(245px,100%); padding: 5px 8px; border: 1px solid var(--card-border); border-radius: 5px; background: var(--bg); font-size: 10px; line-height: 1.6; white-space: normal; overflow-wrap: anywhere; letter-spacing: .4px; }
#settings-access > .settings-row:nth-child(2) { flex-wrap: wrap; gap: 10px; }
#settings-access > .settings-row:nth-child(2) .settings-row-info { flex: 1 1 200px; }
#settings-access > .settings-row:nth-child(2) .settings-row-control { flex: 0 1 auto; }
.settings-editor { display: grid; gap: 8px; padding: 0 0 14px; }
.settings-editor :deep(.el-textarea__inner) { padding: 10px 12px; border-radius: 7px; background: var(--bg); font-size: 11px; line-height: 1.7; }
.settings-editor-footer { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 9px; padding-top: 2px; }
.settings-editor-footer .settings-row-desc { margin-top: 0; font-size: 9px; }
.settings-editor-footer .el-button { font-size: 11px; }
.settings-draft { display: inline-block; padding: 2px 5px; border-radius: 4px; background: var(--badge-amber-soft,var(--primary-soft)); color: var(--badge-amber,var(--warning)); font-size: 9px; }
.settings-page-note .settings-draft { margin-left: 7px; }
.settings-number-control { display: grid; justify-items: end; gap: 7px; min-width: 0; max-width: 100%; padding: 0; }
.settings-number-control .el-input { width: 116px; max-width: 100%; }
.settings-number-control .settings-row-control { justify-content: flex-end; }
.settings-number-control .el-button { min-height: 26px; padding: 5px 9px; font-size: 10px; }
.settings-select { width: 260px; max-width: 100%; min-height: 32px; padding: 5px 27px 5px 10px; border: 1px solid var(--card-border); border-radius: 6px; background: var(--bg); color: var(--text-1); font: inherit; font-size: 11px; cursor: pointer; }
.settings-select:focus-visible { outline: 2px solid var(--primary); outline-offset: 2px; }
.settings-select:disabled { opacity: .65; cursor: not-allowed; }
#settings-system { display: grid; grid-template-columns: 1fr 1fr; gap: 0 20px; }
#settings-system .settings-section-title { grid-column: 1/-1; }
#settings-system .settings-row + .settings-row { border-top: 0; }
#settings-system .settings-row-control { color: var(--text-1); font-size: 11px; font-weight: 550; }
.settings-routing-content { display: grid; gap: 12px; padding: 14px 0; }
.settings-routing-content > .settings-row-control { justify-content: flex-end; padding-top: 2px; }
.management-error { margin-bottom: 12px; overflow-wrap: anywhere; }
@media (min-width: 1200px) {
  #settings-transport, #settings-compatibility { display: grid; grid-template-columns: 1fr 1fr; column-gap: 20px; }
  #settings-transport > .settings-section-title, #settings-compatibility > .settings-section-title { grid-column: 1/-1; }
  #settings-transport > .settings-row, #settings-compatibility > .settings-row { gap: 10px; }
  #settings-transport > .settings-row:nth-child(3), #settings-compatibility > .settings-row:nth-child(3) { border-top: 0; }
  #settings-transport .settings-row-control, #settings-compatibility .settings-row-control { flex: 0 0 36px; justify-content: center; gap: 1px; }
  #settings-transport .settings-switch-hint, #settings-compatibility .settings-switch-hint { text-align: center; }
}
@media (max-width: 1100px) {
  .settings-workspace { grid-template-columns: 161px minmax(0,1fr); gap: 14px; }
  .settings-outline { padding: 10px 5px; }
  .settings-outline nav button { padding: 8px 6px; gap: 7px; }
  .settings-nav-icon { width: 23px; height: 23px; }
  .settings-outline nav button small { font-size: 7px; }
  .settings-panel :deep(.settings-section) { padding: 12px 14px 0; }
  .settings-select { width: 230px; }
  .settings-number-row { flex-wrap: wrap !important; gap: 9px !important; }
  .settings-number-row > .settings-row-info { flex-basis: 100% !important; }
  .settings-number-control { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: center; width: 100%; gap: 8px; }
  .settings-number-control > .settings-row-desc { flex-basis: 100%; }
}
@media (max-width: 850px) {
  .settings-workspace { grid-template-columns: minmax(0,1fr); gap: 12px; }
  .settings-outline { position: static; padding: 0; border: 0; border-radius: 0; background: transparent; }
  .settings-outline-label,.settings-outline-foot { display: none; }
  .settings-outline nav { flex-direction: row; gap: 6px; padding-bottom: 4px; overflow-x: auto; scrollbar-width: thin; }
  .settings-outline nav button { flex: 0 0 auto; width: auto; min-height: 34px; padding: 5px 9px; border-color: var(--card-border); background: var(--card-bg); border-radius: 6px; }
  .settings-outline nav button strong { font-size: 10px; }
  .settings-outline nav button small,.settings-nav-arrow { display: none; }
  .settings-nav-icon { width: 20px; height: 20px; font-size: 11px; }
  .settings-panel :deep(.settings-section) { scroll-margin-top: 14px; }
}
@media (max-width: 600px) {
	#settings-prompt > .settings-row:nth-child(3) { flex-wrap: wrap; }
	#settings-prompt > .settings-row:nth-child(3) .settings-row-info { flex-basis: 100%; }
  .settings-panel :deep(.settings-row) { gap: 10px; }
  .settings-panel :deep(.settings-row-info) { flex-basis: 65%; }
  .settings-panel :deep(.settings-row-control) { gap: 6px; }
  .settings-editor-footer > .settings-row-control { width: 100%; justify-content: flex-end; }
  .settings-select-control { align-items: flex-start; flex-direction: column; }
  .settings-select { width: 100%; }
  #settings-access > .settings-row:nth-child(2) .settings-row-control { width: 100%; }
  #settings-system { gap: 0 12px; }
  #settings-system .settings-row { flex-wrap: wrap; gap: 7px; }
  #settings-system .settings-row-info { flex-basis: 100%; }
  .settings-section-badge { font-size: 7px; padding: 3px 5px; }
  .settings-key { max-width: 100%; }
  .settings-number-control .el-input { width: 103px; }
}
</style>
