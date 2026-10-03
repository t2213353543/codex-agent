<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { formatTime } from '../utils/format'
import { redactImportMessage } from '../utils/importAccounts'

const props = defineProps({ modelValue: Boolean, proxies: { type: Array, default: () => [] } })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const busy = ref(false), error = ref(''), baseURL = ref(''), username = ref(''), password = ref(''), proxyID = ref(''), allowHTTP = ref(false)
const preview = ref(null), selected = ref([]), syncProxies = ref(false), allowIdentity = ref(false), results = ref(null), submitted = ref(false)
const visible = computed({ get: () => props.modelValue, set: value => { if (!busy.value) emit('update:modelValue', value) } })
const rows = computed(() => preview.value?.accounts || [])
const validRows = computed(() => rows.value.filter(row => row.status === 'ready'))
const allSelected = computed(() => validRows.value.length > 0 && validRows.value.every(row => selected.value.includes(row.source_id)))
watch(busy, value => emit('busy', value))
watch(() => props.modelValue, value => { if (value) { error.value = ''; preview.value = null; selected.value = []; results.value = null; submitted.value = false; password.value = ''; syncProxies.value = false; allowIdentity.value = false } else password.value = '' })
function selectPage(value) { selected.value = value ? validRows.value.slice(0, 100).map(row => row.source_id) : [] }
function select(row, checked) { selected.value = checked ? [...new Set([...selected.value, row.source_id])].slice(0, 100) : selected.value.filter(id => id !== row.source_id) }
async function load() {
  if (busy.value || !username.value.trim() || !password.value) return
  error.value = ''
  let url
  try { url = new URL(baseURL.value); if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password) throw new Error(); if (url.protocol === 'http:' && !allowHTTP.value) { error.value = 'HTTP 会明文传输登录信息，必须明确勾选后才能连接'; return } }
  catch { error.value = '请输入有效的 CRS HTTP(S) 地址，不能在 URL 中包含账号密码'; return }
  busy.value = true
  const originalPassword = password.value
  try {
    const data = await accountManagement.crsPreview({ base_url: url.href, username: username.value.trim(), password: originalPassword, ...(proxyID.value ? { proxy_id: proxyID.value } : {}), ...(allowHTTP.value ? { allow_insecure_http: true } : {}) })
    if (!data?.preview_id || !Array.isArray(data.accounts) || data.scope !== 'openai_oauth_only') throw new Error('服务器未返回有效的 Codex 同步预览')
    preview.value = data; selected.value = []; results.value = null; submitted.value = false
    password.value = ''
  } catch (caught) { error.value = redactImportMessage(caught.message || '读取 CRS 失败', [{ payload: { password: originalPassword, access_token: originalPassword } }]) }
  finally { busy.value = false }
}
async function sync() {
  if (busy.value || !preview.value || !selected.value.length || results.value) return
  if (Number(preview.value.expires_at) * 1000 <= Date.now()) { error.value = '预览已过期，请重新连接 CRS 获取预览'; return }
  busy.value = true; error.value = ''; submitted.value = true
  try {
    const response = await accountManagement.crsSync({ preview_id: preview.value.preview_id, selected_account_ids: [...selected.value], sync_proxies: syncProxies.value, allow_identity_change: allowIdentity.value })
    if (!Array.isArray(response.results) || response.results.length !== selected.value.length) throw new Error('服务器返回的同步结果不完整，请核对账号列表')
    results.value = response
    emit('updated')
  } catch (caught) { error.value = caught.message || '同步结果未确认；请用相同选择重读结果，不要重新选择后重复提交' }
  finally { busy.value = false }
}
const status = { added: '已新增', updated: '凭据已更新', duplicate: '重复跳过', failed: '失败' }
</script>

<template>
  <el-dialog v-model="visible" title="从 CRS 同步 Codex 账号" width="min(820px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!busy" :show-close="!busy" @closed="password = ''" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="intro">预览远端 CRS 中的 OpenAI OAuth 账号后选择同步。不会导入 Claude、Gemini 或 API key 类型，也不会隐式刷新令牌或发送推理。</p><el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <el-form v-if="!preview" label-position="top" :disabled="busy"><el-form-item label="CRS 地址"><el-input v-model="baseURL" aria-label="CRS 地址" placeholder="https://crs.example.com" autocomplete="off" /></el-form-item><div class="crs-grid"><el-form-item label="用户名"><el-input v-model="username" aria-label="CRS 用户名" autocomplete="off" /></el-form-item><el-form-item label="密码"><el-input v-model="password" type="password" show-password aria-label="CRS 密码" autocomplete="new-password" /></el-form-item></div><el-form-item label="连接代理"><el-select v-model="proxyID" clearable class="full-width" placeholder="直连" aria-label="CRS 连接代理"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select></el-form-item><el-checkbox v-model="allowHTTP" class="wrap-checkbox">允许使用 HTTP 明文连接（仅在确认网络可信时开启）</el-checkbox><div class="crs-toolbar"><el-button type="primary" :loading="busy" :disabled="!baseURL || !username.trim() || !password" @click="load">连接并预览</el-button></div></el-form>
    <template v-else><p class="muted">来源 {{ preview.source }} · 共 {{ preview.total }} 个 · 预览有效至 {{ formatTime(preview.expires_at) }}</p><div class="crs-toolbar"><el-checkbox :model-value="allSelected" :indeterminate="selected.length > 0 && !allSelected" :disabled="busy || submitted" @change="selectPage">选择可同步项（最多 100 个）</el-checkbox><span class="muted">已选 {{ selected.length }}</span></div><div class="crs-rows"><article v-for="row in rows" :key="row.source_id"><el-checkbox :model-value="selected.includes(row.source_id)" :disabled="busy || submitted || row.status !== 'ready' || selected.length >= 100 && !selected.includes(row.source_id)" :aria-label="`同步 ${row.name}`" @change="select(row, $event)" /><div><strong>{{ row.name }}</strong><p class="muted">{{ row.target_id ? '更新已关联账号凭据' : '创建新账号' }} · {{ row.enabled ? '已启用' : '已停用' }} · {{ row.has_refresh_token ? '含刷新令牌' : '无刷新令牌' }} · {{ row.has_proxy ? '有来源代理' : '直连' }}</p><p v-if="row.error" class="row-error">{{ row.error }}</p></div></article></div><el-checkbox v-model="syncProxies" :disabled="busy || submitted" class="wrap-checkbox">为新账号创建或复用来源代理；已有关联账号代理保持原配置</el-checkbox><el-checkbox v-model="allowIdentity" :disabled="busy || submitted" class="wrap-checkbox">允许已关联账号更换上游登录身份</el-checkbox><p class="muted">已有关联账号只替换凭据，并保留管理配置。预览只能提交同一组选项；网络中断后重试会读取同一操作结果。</p><el-table v-if="results" :data="results.results" size="small" max-height="280"><el-table-column label="来源账号" min-width="150"><template #default="{ row }">{{ rows.find(item => item.source_id === row.source_id)?.name || row.source_id }}</template></el-table-column><el-table-column label="结果" width="110"><template #default="{ row }">{{ status[row.status] || row.status }}</template></el-table-column><el-table-column prop="code" label="说明" min-width="160" /></el-table><p v-if="results">成功 {{ results.succeeded }} · 失败 {{ results.failed }}</p></template>
    <template #footer><el-button :disabled="busy" @click="visible = false">关闭</el-button><el-button v-if="preview" type="primary" :loading="busy" :disabled="!selected.length || Boolean(results)" @click="sync">{{ submitted && !results ? '重读相同同步结果' : `确认同步 ${selected.length} 个账号` }}</el-button></template>
  </el-dialog>
</template>

<style scoped>.intro { margin-top: 0; line-height: 1.7; font-size: 13px; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }.feedback { margin-bottom: 14px; }.crs-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }.full-width { width: 100%; }.crs-toolbar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin: 16px 0; }.crs-rows { max-height: 360px; overflow-y: auto; margin: 14px 0; border-block: 1px solid var(--card-border); }.crs-rows article { display: flex; gap: 12px; align-items: flex-start; padding: 12px 0; border-bottom: 1px solid var(--card-border); }.crs-rows article > div { min-width: 0; }.crs-rows strong { font-size: 13px; overflow-wrap: anywhere; }.crs-rows p { margin: 4px 0; }.row-error { color: var(--danger); font-size: 12px; overflow-wrap: anywhere; }.wrap-checkbox { display: flex; white-space: normal; height: auto; margin: 12px 0; }.wrap-checkbox :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }@media(max-width: 550px) { .crs-grid { grid-template-columns: 1fr; gap: 0; } }</style>
