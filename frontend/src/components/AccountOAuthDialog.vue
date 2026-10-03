<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { defaultImportConfiguration, validateAccountConfiguration } from '../utils/accountFields'
import { formatTime } from '../utils/format'
import { redactImportMessage } from '../utils/importAccounts'

const props = defineProps({ modelValue: Boolean, account: Object, groups: { type: Array, default: () => [] }, proxies: { type: Array, default: () => [] } })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const working = ref(false)
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!working.value) emit('update:modelValue', value) } })
const target = ref(null)
const name = ref('')
const method = ref('browser')
const proxyID = ref('')
const defaults = ref(defaultImportConfiguration())
const groupIDs = ref([])
const notes = ref(''), tags = ref([]), enabled = ref(true)
const source = ref('')
const clientID = ref('')
const code = ref('')
const state = ref('')
const session = ref(null)
const allowIdentityChange = ref(false)
const error = ref('')
const info = ref('')
const clock = ref(Date.now())
const expired = computed(() => session.value?.expires_at && Number(session.value.expires_at) * 1000 <= clock.value)
const ready = computed(() => session.value?.phase === 'ready' && !expired.value)
const methods = { browser: '浏览器 OAuth 授权', refresh_token: '刷新令牌', mobile_refresh_token: '移动端刷新令牌', session: 'Codex session / auth.json', pat: 'Codex PAT' }
const authURL = computed(() => {
  try { const url = new URL(session.value?.auth_url); return url.protocol === 'https:' && url.hostname === 'auth.openai.com' ? url.href : '' }
  catch { return '' }
})
const timer = setInterval(() => { clock.value = Date.now() }, 1000)
function eraseInput() { source.value = ''; clientID.value = ''; code.value = ''; state.value = '' }
function cancelSession() {
  const id = session.value?.session_id
  session.value = null
  eraseInput()
  if (id) accountManagement.oauthCancel(id).catch(() => { /* Server TTL also expires abandoned encrypted sessions. */ })
}
watch(() => props.modelValue, (value) => {
  if (!value) { cancelSession(); return }
  target.value = props.account ? { ...props.account } : null
  name.value = ''
  method.value = 'browser'
  proxyID.value = ''
  defaults.value = defaultImportConfiguration()
  groupIDs.value = []
  notes.value = ''; tags.value = []; enabled.value = true
  error.value = ''; info.value = ''; allowIdentityChange.value = false
})
watch(working, (value) => emit('busy', value))
watch(method, () => { cancelSession(); error.value = ''; info.value = ''; allowIdentityChange.value = false })
onUnmounted(() => { clearInterval(timer); cancelSession() })
const basePayload = () => ({ ...(proxyID.value ? { proxy_id: proxyID.value } : {}), ...(target.value ? { target_id: target.value.id, version: target.value.version } : {}) })
async function start() {
  if (working.value) return
  cancelSession()
  error.value = ''; info.value = ''; working.value = true
  try {
    session.value = await accountManagement.oauthStart(basePayload())
    if (!session.value?.session_id || !authURL.value) throw new Error('服务器未返回有效的官方授权链接')
  } catch (caught) { error.value = caught.message || '创建授权会话失败' }
  finally { working.value = false }
}
async function copyURL() {
  try { await navigator.clipboard.writeText(authURL.value); info.value = '授权链接已复制' }
  catch { info.value = ''; error.value = '复制失败，请使用下方链接打开授权页面' }
}
async function exchange() {
  if (working.value || !session.value?.session_id || !code.value.trim() || expired.value) return
  error.value = ''; info.value = ''; working.value = true
  try {
    const result = await accountManagement.oauthExchange({ session_id: session.value.session_id, code: code.value.trim(), ...(state.value.trim() ? { state: state.value.trim() } : {}) })
    session.value = { ...session.value, ...result }
    if (session.value.phase !== 'ready') throw new Error('授权尚未完成，请核对回调内容')
    eraseInput()
  } catch (caught) { error.value = '授权交换失败：' + (caught.status === 409 ? '会话状态已变化，请重新生成链接' : redactImportMessage(caught.message || '请重试', [{ payload: { access_token: code.value, refresh_token: state.value } }])) }
  finally { working.value = false }
}
async function importCredential() {
  if (working.value || !source.value.trim()) return
  error.value = ''; info.value = ''
  let payload = { ...basePayload(), method: method.value }
  try {
    if (method.value === 'session') {
      try { payload.credentials = JSON.parse(source.value) } catch { throw new Error('session / auth.json 不是有效 JSON') }
      if (!payload.credentials || typeof payload.credentials !== 'object' || Array.isArray(payload.credentials)) throw new Error('请提供单个账号的 session 或 auth.json 对象')
    } else if (method.value === 'pat') payload.access_token = source.value.trim()
    else {
      payload.refresh_token = source.value.trim()
      if (method.value === 'mobile_refresh_token' && !clientID.value.trim()) throw new Error('移动端令牌必须填写签发时的原 client_id')
      if (clientID.value.trim()) payload.client_id = clientID.value.trim()
    }
  } catch (caught) { error.value = caught.message; return }
  working.value = true
  try {
    const previous = session.value?.session_id
    const result = await accountManagement.oauthImport(payload)
    if (!result?.session_id || result.phase !== 'ready') throw new Error('服务器未确认凭据就绪')
    session.value = result
    eraseInput()
    if (previous && previous !== result.session_id) accountManagement.oauthCancel(previous).catch(() => {})
  } catch (caught) { error.value = redactImportMessage(caught.message || '凭据校验失败', [{ payload }]) }
  finally { working.value = false; payload = null }
}
async function apply() {
  if (working.value || !ready.value || session.value.target_changed || session.value.identity_change && !allowIdentityChange.value) return
  error.value = ''; info.value = ''
  if (!target.value) {
    if (!name.value.trim() || new TextEncoder().encode(name.value.trim()).length > 120) { error.value = '账号名称不能为空，且最多 120 个 UTF-8 字节'; return }
    error.value = validateAccountConfiguration(defaults.value)
    if (error.value) return
  }
  working.value = true
  try {
    const result = await accountManagement.oauthApply({ session_id: session.value.session_id, ...(target.value ? {} : { name: name.value.trim(), notes: notes.value, tags: [...tags.value], enabled: enabled.value, defaults: { ...defaults.value, groups: [...groupIDs.value], proxies: proxyID.value ? [proxyID.value] : [] } }), ...(allowIdentityChange.value ? { allow_identity_change: true } : {}) })
    if (result?.ok !== true || !result.id) throw new Error('服务器未确认账号保存成功')
    session.value = null
    eraseInput()
    emit('updated', result.id)
    emit('update:modelValue', false)
  } catch (caught) { error.value = caught.status === 409 ? '账号名称、版本或身份发生冲突。授权凭据仍在会话中；请核对后重试。' : caught.message || '保存账号失败，授权会话仍可重试' }
  finally { working.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" :title="target ? 'OAuth 重新授权' : '添加 Codex 账号'" width="min(700px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!working" :show-close="!working" @closed="eraseInput" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="oauth-intro">{{ target ? `为「${target.name}」重新登录，账号 ID、管理配置与使用记录保持关联。` : '选择授权方式，确认登录凭据后保存账号。' }}</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <el-alert v-if="info" :title="info" type="info" :closable="false" class="feedback" />
    <el-alert v-if="expired" title="授权会话已过期，请重新开始授权。输入过期回调不会保存账号。" type="warning" :closable="false" class="feedback" />
    <el-alert v-if="session?.target_changed" title="目标账号在授权期间已被修改，请关闭窗口、刷新账号后重新授权。" type="warning" :closable="false" class="feedback" />
    <el-form label-position="top" :disabled="working">
      <div class="oauth-two-columns"><el-form-item label="授权方式"><el-select v-model="method" aria-label="Codex 授权方式" class="full-width"><el-option v-for="(label, value) in methods" :key="value" :label="label" :value="value" /></el-select></el-form-item><el-form-item label="授权代理"><el-select v-model="proxyID" clearable aria-label="OAuth 授权代理" :placeholder="target ? '沿用账号现有可用代理' : '直连'" class="full-width" :disabled="Boolean(session) && !expired"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" :disabled="proxy.enabled === false" /></el-select></el-form-item></div>
      <template v-if="method === 'browser' && !ready">
        <el-button :loading="working" @click="start">{{ session ? '重新生成授权链接' : '生成授权链接' }}</el-button>
        <div v-if="authURL && !expired" class="authorization-link"><a :href="authURL" target="_blank" rel="noopener noreferrer">打开 OpenAI 授权页面</a><el-button link @click="copyURL">复制链接</el-button><p class="muted">完成登录后，复制浏览器地址栏中的完整回调地址。即使 localhost 页面无法打开，也可以复制该地址继续。</p><p class="muted">会话有效期至 {{ formatTime(session.expires_at) }}</p></div>
        <template v-if="session && !expired"><el-form-item label="完整回调地址或授权码"><el-input v-model="code" type="textarea" :rows="3" aria-label="OAuth 回调地址或授权码" autocomplete="off" placeholder="粘贴 http://localhost:…/auth/callback?code=…&state=…" /></el-form-item><el-form-item label="state（粘贴完整回调地址时可留空）"><el-input v-model="state" aria-label="OAuth state" autocomplete="off" /></el-form-item><el-button type="primary" :disabled="!code.trim()" :loading="working" @click="exchange">确认授权回调</el-button></template>
      </template>
      <template v-else-if="method !== 'browser' && !ready">
        <el-form-item :label="methods[method]"><el-input v-model="source" type="textarea" :rows="method === 'session' ? 7 : 4" aria-label="OAuth 凭据内容" autocomplete="off" :placeholder="method === 'session' ? '粘贴包含访问令牌的 session 或 auth.json；支持 OAuth tokens 附带的 agent_identity 元数据；不支持纯 session cookie 或单独 Agent 私钥' : method === 'pat' ? '粘贴 Codex PAT' : '粘贴 refresh_token'" /></el-form-item>
        <el-form-item v-if="method === 'refresh_token' || method === 'mobile_refresh_token'" :label="method === 'mobile_refresh_token' ? '原 client_id（必填）' : 'client_id（可选）'"><el-input v-model="clientID" aria-label="OAuth client_id" autocomplete="off" /></el-form-item>
        <p v-if="method === 'mobile_refresh_token'" class="muted">使用令牌原始签发客户端的 client_id 完成交换。此授权方式不改变当前 PowerPoint 推理入口。</p>
        <el-button type="primary" :disabled="!source.trim()" :loading="working" @click="importCredential">校验并准备凭据</el-button>
      </template>
      <div v-if="ready" class="oauth-ready"><strong>凭据已准备就绪，尚未写入账号</strong><p>刷新令牌：{{ session.has_refresh_token ? '可用' : '无' }} · 令牌到期：{{ formatTime(session.expires_at_token) }}</p><p class="muted">会话有效期至 {{ formatTime(session.expires_at) }}。保存失败可以重试，无需重复授权。</p><el-checkbox v-if="session.identity_change" v-model="allowIdentityChange">我确认将此账号更换为新的上游登录身份</el-checkbox></div>
      <template v-if="ready && !target">
        <el-form-item label="账号名称"><el-input v-model="name" aria-label="新建 Codex 账号名称" placeholder="为此账号命名" /></el-form-item><el-form-item label="分组"><el-select v-model="groupIDs" multiple class="full-width" aria-label="新建 Codex 账号分组"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select></el-form-item>
        <el-form-item label="备注"><el-input v-model="notes" type="textarea" :rows="2" aria-label="OAuth 新账号备注" /></el-form-item><el-form-item label="标签"><el-select v-model="tags" multiple filterable allow-create default-first-option class="full-width" aria-label="OAuth 新账号标签" /></el-form-item><el-form-item label="创建后启用"><el-switch v-model="enabled" aria-label="OAuth 新账号启用" /></el-form-item>
        <details class="oauth-defaults"><summary>并发、优先级与用量窗口</summary><div class="oauth-two-columns"><el-form-item label="并发容量"><el-input-number v-model="defaults.concurrency" :min="1" :max="1000" :precision="0" aria-label="OAuth 新账号并发" /></el-form-item><el-form-item label="优先级"><el-input-number v-model="defaults.priority" :precision="0" aria-label="OAuth 新账号优先级" /></el-form-item><el-form-item label="权重"><el-input-number v-model="defaults.weight" :min="1" :max="1000" :precision="0" aria-label="OAuth 新账号权重" /></el-form-item><el-form-item label="Token 参考额度"><el-input-number v-model="defaults.quota" :min="0" :max="Number.MAX_SAFE_INTEGER" :precision="0" aria-label="OAuth 新账号额度" /></el-form-item><el-form-item label="统计窗口（天）"><el-input-number v-model="defaults.window_days" :min="1" :max="365" :precision="0" aria-label="OAuth 新账号窗口" /></el-form-item></div></details>
      </template>
    </el-form>
    <template #footer><el-button :disabled="working" @click="visible = false">取消</el-button><el-button type="primary" :disabled="!ready || session?.target_changed || session?.identity_change && !allowIdentityChange" :loading="working" @click="apply">{{ target ? '保存重新授权' : '创建账号' }}</el-button></template>
  </el-dialog>
</template>

<style scoped>.oauth-intro { margin-top: 0; line-height: 1.7; overflow-wrap: anywhere; }.full-width { width: 100%; }.feedback { margin-bottom: 14px; }.oauth-two-columns { display: grid; grid-template-columns: 1fr 1fr; gap: 0 16px; }.oauth-two-columns > * { min-width: 0; }.authorization-link { padding: 14px 0; }.authorization-link a { color: var(--el-color-primary); margin-right: 16px; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.oauth-ready { padding: 14px; margin: 16px 0; border: 1px solid var(--el-color-success-light-7); background: var(--el-color-success-light-9); border-radius: 6px; font-size: 13px; }.oauth-ready p { margin: 8px 0; }.oauth-ready .el-checkbox { white-space: normal; height: auto; }.oauth-ready :deep(.el-checkbox__label) { white-space: normal; line-height: 1.6; }.oauth-defaults summary { padding: 12px 0; cursor: pointer; font-size: 13px; }.oauth-defaults .el-input-number { width: 100%; }@media(max-width: 520px) { .oauth-two-columns { grid-template-columns: 1fr; } }</style>
