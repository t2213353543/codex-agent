<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { lastUsedScopeLabel } from '../utils/accountFields'
import { formatTime } from '../utils/format'
import PowerPointService from './PowerPointService.vue'

const props = defineProps({ account: Object })
const data = ref(null), loading = ref(false), error = ref(''), model = ref(''), clock = ref(Date.now())
let sequence = 0, controller, receivedAt = 0
const timer = setInterval(() => { clock.value = Date.now() }, 1000)
const now = computed(() => data.value ? data.value.checked_at + Math.max(0, clock.value - receivedAt) : clock.value)
const switches = { websocket_enabled: 'WebSocket', http_stream_enabled: 'HTTP 流式', http_json_enabled: 'HTTP JSON', compact_enabled: 'Compact', models_enabled: '模型目录', compatibility_normalization_enabled: '客户端参数兼容', automatic_token_refresh_enabled: '到期自动刷新', upstream_401_recovery_enabled: '401 凭据恢复' }
const kindLabel = entry => ({ rate_limit: '上游限流冷却', capacity_guard: '冷却存储容量保护', oauth_refresh_http: 'OAuth 刷新等待', oauth_refresh_connection: 'OAuth 连接重试等待', oauth_refresh_invalid: 'OAuth 响应重试等待' }[entry.kind] || '已记录的等待状态')
const scopeLabel = scope => scope === 'connection' ? '账号连接' : scope === 'models' ? '模型目录接口' : scope === 'all' ? '全部账号选择' : scope === 'token_refresh' ? '令牌刷新' : scope.startsWith('model:') ? `模型 ${scope.slice(6)}` : scope
const countdown = until => until <= now.value ? '截止已过，请重新读取' : `剩余 ${Math.ceil((until - now.value) / 1000)} 秒`
watch(() => [props.account?.id, props.account?.version], () => { model.value = ''; data.value = null; load() }, { immediate: true })
onUnmounted(() => { sequence++; controller?.abort(); clearInterval(timer) })
async function load() {
  controller?.abort(); const current = ++sequence
  data.value = null; error.value = ''; loading.value = false
  if (!props.account?.id) return
  const requestedModel = model.value.trim()
  if (new TextEncoder().encode(requestedModel).length > 200) { error.value = '模型 ID 最多 200 个 UTF-8 字节'; return }
  controller = new AbortController(); loading.value = true
  try {
    const result = await accountManagement.runtime(props.account.id, requestedModel, { signal: controller.signal })
    if (current !== sequence) return
    if (result.id !== props.account.id || !Number.isFinite(result.checked_at) || !result.admission || !Array.isArray(result.cooldowns?.entries) || !result.effective_policy || !result.last_used) throw new Error('运行状态返回格式无效')
    receivedAt = Date.now(); clock.value = receivedAt; data.value = result
  } catch (caught) { if (current === sequence && caught.name !== 'AbortError') error.value = caught.message || '运行状态读取失败' }
  finally { if (current === sequence) loading.value = false }
}
</script>

<template>
  <section class="account-runtime-panel" aria-label="网关实际运行状态">
    <div class="runtime-heading"><h3>网关实际运行状态</h3><el-button size="small" :loading="loading" @click="load">读取运行状态</el-button></div>
    <p class="runtime-note">只读当前服务的并发、已有冷却记录和系统设置，不会发送上游请求。各状态的适用范围不同，不能据此保证下一次请求可用。</p>
    <form class="runtime-model" @submit.prevent="load"><label for="account-runtime-model">检查指定模型的冷却</label><div><el-input id="account-runtime-model" v-model="model" placeholder="输入请求实际使用的模型 ID（可选）" maxlength="200" clearable @keydown.enter.prevent="load" /><el-button native-type="submit" :loading="loading">读取</el-button></div></form>
    <el-alert v-if="error" :title="error" type="error" :closable="false" />
    <p v-if="loading" role="status" class="runtime-note">正在读取运行状态…</p>
    <template v-if="data">
      <p class="runtime-note">读取时间 {{ formatTime(data.checked_at / 1000) }}<span v-if="data.version !== account.version"> · 账号版本已变化，以下采用服务端版本 {{ data.version }}</span></p>
      <dl class="runtime-fields">
        <div><dt>本机并发</dt><dd>{{ data.admission.in_flight }} / {{ data.admission.limit }}<span :class="data.admission.saturated ? 'runtime-warning' : ''"> · {{ data.admission.saturated ? '容量已占满，暂不能接纳新请求' : '当前未占满' }}</span><small>仅当前服务进程的瞬时值，不包含其他实例。</small></dd></div>
        <div><dt>最近业务请求</dt><dd>{{ data.last_used.at ? formatTime(data.last_used.at) : '此范围内没有匹配记录' }}<small>{{ lastUsedScopeLabel(data.last_used.scope) }}；按稳定账号 ID 匹配，含成功、失败或取消请求，不含独立管理测试。</small></dd></div>
      </dl>
      <el-alert v-if="data.cooldowns.shared_state === 'unavailable'" title="共享冷却状态未取得，不能判断共享等待是否已经结束；请稍后重新读取。" type="warning" :closable="false" />
      <ul v-if="data.cooldowns.entries.length" class="runtime-cooldowns">
        <li v-for="entry in data.cooldowns.entries" :key="`${entry.kind}:${entry.scope}:${entry.source}`"><strong>{{ kindLabel(entry) }}<span v-if="entry.http_code"> · HTTP {{ entry.http_code }}</span></strong><span>{{ scopeLabel(entry.scope) }} · {{ entry.source === 'shared_redis' ? '共享 Redis 记录' : '当前服务进程记录' }}</span><span>截止 {{ formatTime(entry.until / 1000) }} · {{ countdown(entry.until) }}</span><small v-if="entry.scope === 'token_refresh'">此记录限制刷新重试，不代表当前访问令牌一定不可用。</small></li>
      </ul>
      <p v-else class="runtime-note">{{ data.cooldowns.shared_state === 'unavailable' ? '本机未发现活动冷却；共享状态尚未取得。' : '已读取范围内未发现活动冷却记录。' }}</p>
      <p class="runtime-note">已检查账号连接、模型目录{{ data.cooldowns.model ? `及模型 ${data.cooldowns.model}` : '；尚未指定模型' }}。其他模型的冷却未检查。倒计时仅反映读取时的截止，不会自动清除或确认恢复。</p>
      <details class="runtime-policy"><summary>当前有效的系统策略</summary>
      <p class="runtime-note">来源：全局系统设置，版本 {{ data.effective_policy.settings_version }}。账号没有独立覆盖；其他实例最多存在约 2 秒的设置缓存。</p>
      <PowerPointService :inference="data.effective_policy.inference" />
      <dl class="runtime-fields">
        <div><dt>会话账号绑定</dt><dd>PowerPoint 会话固定开启</dd></div>
        <div v-for="(label, key) in switches" :key="key"><dt>{{ label }}</dt><dd>{{ data.effective_policy[key] ? '系统已启用' : '系统已关闭' }}</dd></div>
        <div><dt>内置提示词</dt><dd>{{ data.effective_policy.builtin_prompt_enabled ? `系统已启用（${data.effective_policy.builtin_prompt_mode}）` : '系统已关闭' }}</dd></div>
        <div><dt>账号实际参数</dt><dd>{{ data.effective_policy.account.enabled ? '账号已启用' : '账号已停用' }} · 并发 {{ data.effective_policy.account.concurrency }} · {{ data.effective_policy.account.has_refresh_token ? '存在刷新令牌' : '无刷新令牌，开启系统刷新仍无法刷新该账号' }}<small>访问令牌到期：{{ data.effective_policy.account.expires_at ? formatTime(data.effective_policy.account.expires_at) : '未提供' }}</small></dd></div>
      </dl>
      </details>
    </template>
  </section>
</template>

<style scoped>
.account-runtime-panel { margin: 24px 0; padding-top: 18px; border-top: 1px solid var(--card-border); }
.runtime-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }.runtime-heading h3,h4 { margin: 0; font-size: 14px; }h4 { margin-top: 22px; }
.runtime-policy { margin-top: 18px; }.runtime-policy summary { cursor: pointer; font-size: 14px; font-weight: 600; padding: 8px 0; }
.runtime-note,small { color: var(--text-3); font-size: 12px; line-height: 1.7; }.runtime-fields { margin: 12px 0; font-size: 12px; }.runtime-fields > div { display: grid; grid-template-columns: 140px minmax(0,1fr); gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--card-border); }.runtime-fields dt { color: var(--text-3); }.runtime-fields dd { margin: 0; overflow-wrap: anywhere; }small,code { display: block; }code { font-size: 11px; margin-top: 5px; }.runtime-model { margin: 14px 0; }.runtime-model label { font-size: 12px; display: block; margin-bottom: 7px; }.runtime-model > div { display: flex; gap: 8px; }.runtime-warning { color: var(--el-color-warning); }.runtime-cooldowns { list-style: none; padding: 0; margin: 12px 0; }.runtime-cooldowns li { display: grid; gap: 5px; border-left: 3px solid var(--el-color-warning); background: var(--el-fill-color-light); margin-bottom: 10px; padding: 12px; font-size: 12px; overflow-wrap: anywhere; }.runtime-cooldowns strong { font-weight: 600; }
@media(max-width:480px) { .runtime-fields > div { grid-template-columns: 98px minmax(0,1fr); }.runtime-model > div { flex-wrap: wrap; }.runtime-model .el-input { width: 100%; } }
</style>
