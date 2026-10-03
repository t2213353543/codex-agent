<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { formatDuration, formatNumber, formatTime } from '../utils/format'
import AccountControlsPanel from './AccountControlsPanel.vue'
import { streamAccountTest } from '../utils/accountTestStream'
import { officialUsageWindows, accountWindowCostLabel, accountWindowCostHint, accountWindowTotalLabel, accountWindowTotalHint } from '../utils/accountOfficialUsage'

const props = defineProps({ modelValue: Boolean, account: Object, initialTab: { type: String, default: 'usage' } })
const emit = defineEmits(['update:modelValue', 'snapshot'])
const controlsBusy = ref(false)
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!controlsBusy.value) emit('update:modelValue', value) } })
const target = ref(null)
const tab = ref('usage')
const snapshots = ref({})
const loading = ref(false)
const checking = ref('')
const error = ref('')
const notice = ref('')
const model = ref('')
const testKind = ref('stream')
const confirmInference = ref(false)
const prompt = ref('')
const streamText = ref('')
const streamPhase = ref('')
let generation = 0
let controller
const tabs = [{ key: 'usage', label: '上游额度' }, { key: 'models', label: '模型目录' }, { key: 'test', label: '连通测试' }, { key: 'controls', label: '隐私与重置' }]
const catalog = computed(() => snapshots.value.models?.ok && Array.isArray(snapshots.value.models.data?.models) ? snapshots.value.models.data.models : [])
const usage = computed(() => snapshots.value.usage?.ok ? snapshots.value.usage.data : null)
const quotaWindows = computed(() => officialUsageWindows(snapshots.value.usage, { additional: true }))
const currentTest = computed(() => snapshots.value[testKind.value])
const percent = value => typeof value === 'number' && Number.isFinite(value) ? `${Number(value.toFixed(1))}%` : '未知'
const progress = value => typeof value === 'number' && Number.isFinite(value) ? Math.min(100, Math.max(0, value)) : 0
const snapshotTime = snapshot => snapshot?.checked_at ? formatTime(snapshot.checked_at) : '尚未查询'
const resetTime = window => window.reset_at ? formatTime(window.reset_at) : '未知'
const boolLabel = (value, yes, no) => value === true ? yes : value === false ? no : '未知'
function stop() { generation++; controller?.abort(); controller = null; loading.value = false; checking.value = '' }
watch(() => props.modelValue, (value) => {
  stop()
  if (!value) return
  target.value = { ...props.account }
  tab.value = tabs.some(item => item.key === props.initialTab) ? props.initialTab : 'usage'
  snapshots.value = {}; error.value = ''; notice.value = ''; model.value = ''; testKind.value = 'stream'; confirmInference.value = false; prompt.value = ''; streamText.value = ''; streamPhase.value = ''
  loadSnapshots()
})
onUnmounted(stop)
async function loadSnapshots() {
  if (!target.value?.id) return
  stop()
  const current = generation
  controller = new AbortController()
  loading.value = true; error.value = ''
  try {
    const result = await accountManagement.upstreamSnapshots(target.value.id, { signal: controller.signal })
    if (current !== generation || !visible.value) return
    if (result.id !== target.value.id || !result.snapshots || typeof result.snapshots !== 'object') throw new Error('服务器返回的账号检查记录无效')
    snapshots.value = result.snapshots
    if (result.persistent !== true) notice.value = '当前检查记录未确认持久保存。'
  } catch (caught) { if (current === generation && caught.name !== 'AbortError') error.value = caught.message || '读取已保存检查记录失败' }
  finally { if (current === generation) loading.value = false }
}
async function check(kind) {
  if (loading.value || checking.value || !target.value?.id) return
  if (['stream', 'compact'].includes(kind) && (!model.value.trim() || !confirmInference.value)) return
  if (kind === 'stream' && new TextEncoder().encode(prompt.value).length > 4096) { error.value = '测试提示词不能超过 4096 个 UTF-8 字节'; return }
  stop()
  const current = generation
  controller = new AbortController()
  checking.value = kind; error.value = ''; notice.value = ''; streamText.value = ''; streamPhase.value = ''
  try {
    const result = kind === 'stream'
      ? await streamAccountTest(target.value.id, { version: target.value.version, model: model.value.trim(), confirm_inference: true, ...(prompt.value.trim() ? { prompt: prompt.value.trim() } : {}) }, { signal: controller.signal, onPhase: value => { if (current === generation) streamPhase.value = value }, onDelta: value => { if (current === generation) streamText.value = (streamText.value + value).slice(-65536) } })
      : await accountManagement.upstreamCheck(target.value.id, { version: target.value.version, kind, ...(kind === 'compact' ? { model: model.value.trim(), confirm_inference: true } : {}) }, { signal: controller.signal })
    if (current !== generation || !visible.value) return
    if (result.id !== target.value.id || result.kind !== kind || typeof result.ok !== 'boolean') throw new Error('服务器未返回有效的检查结果')
    snapshots.value = { ...snapshots.value, [kind]: result }
    emit('snapshot', result)
    if (kind === 'models' && !model.value && result.ok && result.data?.models?.length) model.value = result.data.models[0].id
  } catch (caught) { if (current === generation && caught.name !== 'AbortError') error.value = caught.status === 409 ? '账号在打开窗口后已变更，请关闭后重新打开再检查。' : caught.message || '账号检查失败' }
  finally { if (current === generation) checking.value = '' }
}
function stopWaiting() { stop(); notice.value = '已停止等待本次结果；上游可能已处理请求。稍后可重新读取已保存检查记录。' }
function moveTab(event, index) {
  if (controlsBusy.value || checking.value) return
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length
  tab.value = tabs[next].key
  event.currentTarget.parentElement.children[next]?.focus()
}
</script>

<template>
  <el-dialog v-model="visible" title="账号用量与测试" width="min(800px, calc(100vw - 32px))" class="detail-dialog account-upstream-dialog" :close-on-click-modal="!controlsBusy" :close-on-press-escape="!controlsBusy" :show-close="!controlsBusy" align-center append-to-body destroy-on-close>
    <div class="upstream-header"><strong>{{ target?.name || target?.id }}</strong><span class="muted">管理检查独立保存，不改写历史请求或业务调度状态。</span></div>
    <div class="upstream-tabs" role="tablist" aria-label="账号检查功能"><button v-for="(item, index) in tabs" :id="`upstream-tab-${item.key}`" :key="item.key" type="button" role="tab" :disabled="controlsBusy || Boolean(checking)" :aria-selected="tab === item.key" :aria-controls="`upstream-panel-${item.key}`" :tabindex="tab === item.key ? 0 : -1" @click="tab = item.key" @keydown="moveTab($event, index)">{{ item.label }}</button></div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <el-alert v-if="notice" :title="notice" type="info" :closable="false" class="feedback" />
    <p v-if="loading" role="status" class="muted">正在读取已保存的检查记录…</p>
    <section v-show="tab === 'usage'" id="upstream-panel-usage" role="tabpanel" aria-labelledby="upstream-tab-usage" class="upstream-panel">
      <div class="section-heading"><h3>上游额度</h3><el-button :loading="checking === 'usage'" :disabled="loading || Boolean(checking)" @click="check('usage')">查询上游额度</el-button></div>
      <p class="muted">只读查询 ChatGPT 账号的订阅额度，不发送模型推理。这里的账号额度信息与 PowerPoint 的模型目录、连通测试分开记录。打开此页面只读取已保存快照。</p>
      <template v-if="snapshots.usage"><p class="snapshot-meta">{{ snapshotTime(snapshots.usage) }} · 来源 {{ snapshots.usage.source }} <el-tag v-if="snapshots.usage.stale" type="warning" size="small">快照已陈旧</el-tag></p><el-alert v-if="!snapshots.usage.ok" :title="snapshots.usage.error?.message || '上游额度查询未成功'" type="error" :closable="false" class="feedback" /></template>
      <p v-else class="empty-state">尚无上游额度快照。点击查询后显示窗口用量与重置时间。</p>
      <template v-if="usage">
        <dl class="upstream-facts"><div><dt>套餐</dt><dd>{{ usage.plan_type || '未知' }}</dd></div><div><dt>上游允许请求</dt><dd>{{ boolLabel(usage.rate_limit?.allowed, '是', '否') }}</dd></div><div><dt>已达到额度</dt><dd>{{ boolLabel(usage.rate_limit?.limit_reached, '是', '否') }}</dd></div></dl>
        <div v-if="quotaWindows.length" class="quota-windows"><div v-for="(window, index) in quotaWindows" :key="index" class="quota-window"><div class="quota-title"><span>{{ window.label }}</span><strong>{{ percent(window.used_percent) }}</strong></div><progress v-if="typeof window.used_percent === 'number'" :value="progress(window.used_percent)" max="100" :aria-label="`${window.label}使用比例`" /><p v-if="window.local_cost" :title="accountWindowCostHint(window.local_cost)">{{ accountWindowCostLabel(window.local_cost) }}</p><p v-if="accountWindowTotalLabel(window)" class="official-usage-total" :title="accountWindowTotalHint(window)">{{ accountWindowTotalLabel(window) }}</p><p class="muted">重置时间：{{ resetTime(window) }}</p></div></div><p v-else class="empty-state">上游没有提供窗口数据，不能据此判断额度为零。</p>
        <dl v-if="usage.credits" class="upstream-facts"><div><dt>有可用积分</dt><dd>{{ boolLabel(usage.credits.has_credits, '是', '否') }}</dd></div><div><dt>无限积分</dt><dd>{{ boolLabel(usage.credits.unlimited, '是', '否') }}</dd></div><div><dt>上游积分余额</dt><dd>{{ usage.credits.balance ?? '未知' }}</dd></div></dl>
      </template>
      <p class="muted">账号列表的用量窗口与这里使用相同的官方数据。本地请求与 Token 记录在账号统计中查看。</p>
    </section>
    <section v-show="tab === 'models'" id="upstream-panel-models" role="tabpanel" aria-labelledby="upstream-tab-models" class="upstream-panel">
      <div class="section-heading"><h3>模型目录</h3><el-button :loading="checking === 'models'" :disabled="loading || Boolean(checking)" @click="check('models')">读取模型目录</el-button></div><p class="muted">此目录来自当前账号的 PowerPoint 服务。目录中列出模型不代表已经通过实际推理测试。</p>
      <p v-if="snapshots.models" class="snapshot-meta">{{ snapshotTime(snapshots.models) }} · {{ catalog.length }} 个模型 <el-tag v-if="snapshots.models.stale" size="small" type="warning">快照已陈旧</el-tag></p>
      <el-alert v-if="snapshots.models && !snapshots.models.ok" :title="snapshots.models.error?.message || '读取模型目录失败'" type="error" :closable="false" class="feedback" />
      <ul v-if="catalog.length" class="model-list"><li v-for="item in catalog" :key="item.id"><div><strong>{{ item.display_name || item.id }}</strong><code>{{ item.id }}</code></div><span v-if="item.context_window != null" class="muted">上下文 {{ formatNumber(item.context_window) }}</span><el-button size="small" @click="model = item.id; tab = 'test'; confirmInference = false">选择测试</el-button></li></ul><p v-else class="empty-state">{{ snapshots.models?.ok ? '上游目录为空' : '尚无可展示的模型目录' }}</p>
    </section>
    <section v-show="tab === 'test'" id="upstream-panel-test" role="tabpanel" aria-labelledby="upstream-tab-test" class="upstream-panel">
      <h3>显式连通测试</h3>
      <el-form label-position="top"><el-form-item label="真实模型 ID"><el-select v-model="model" filterable allow-create default-first-option class="full-width" aria-label="账号测试模型" placeholder="选择上游目录或输入真实模型 ID" :disabled="Boolean(checking)"><el-option v-for="item in catalog" :key="item.id" :label="item.id" :value="item.id" /></el-select></el-form-item><el-form-item label="测试模式"><el-select v-model="testKind" aria-label="账号测试模式" class="full-width" :disabled="Boolean(checking)"><el-option label="普通流式响应" value="stream" /><el-option label="compact 压缩响应" value="compact" /></el-select></el-form-item><el-checkbox v-model="confirmInference" :disabled="Boolean(checking)" class="inference-confirm">我确认此测试会实际发送请求，可能消耗上游额度</el-checkbox></el-form>
      <el-input v-if="testKind === 'stream'" v-model="prompt" type="textarea" :rows="2" aria-label="测试提示词" placeholder="可选：自定义短测试提示词（最多 4096 字节）" :disabled="Boolean(checking)" class="test-prompt" />
      <div class="test-actions"><el-button type="primary" :loading="['stream', 'compact'].includes(checking)" :disabled="!model.trim() || !confirmInference || loading || Boolean(checking)" @click="check(testKind)">开始测试</el-button><el-button v-if="checking" @click="stopWaiting">停止等待</el-button><el-button v-else :disabled="loading" @click="loadSnapshots">重新读取检查记录</el-button></div>
      <div v-if="checking === 'stream' || streamText" class="stream-output" role="status"><span class="muted">{{ checking === 'stream' ? streamPhase === 'streaming' ? '正在接收上游响应…' : '正在连接上游…' : '本次流式输出' }}</span><pre>{{ streamText || '等待内容' }}</pre></div>
      <div v-if="currentTest" class="test-result" aria-live="polite"><div class="section-heading"><strong>{{ currentTest.ok ? '测试通过' : '测试失败' }}</strong><el-tag :type="currentTest.ok ? 'success' : 'danger'" size="small">{{ currentTest.http_code ? `HTTP ${currentTest.http_code}` : '未取得 HTTP 响应' }}</el-tag></div><p class="snapshot-meta">{{ snapshotTime(currentTest) }} · {{ formatDuration(currentTest.latency_ms) }} <el-tag v-if="currentTest.stale" size="small" type="warning">历史快照</el-tag></p><p class="muted">测试模型：{{ currentTest.model || '该历史快照未记录模型' }}</p><pre v-if="currentTest.error">{{ currentTest.error.message || currentTest.error.code }}</pre><pre v-if="currentTest.data?.text">{{ currentTest.data.text }}</pre><p v-if="currentTest.data?.compaction_found">已验证上游返回有效的 compaction 结果。</p><p v-if="currentTest.data?.usage" class="muted">输入 {{ formatNumber(currentTest.data.usage.input_tokens) }} · 输出 {{ formatNumber(currentTest.data.usage.output_tokens) }} Token</p></div>
      <p v-else class="empty-state">该测试模式尚无已保存结果。</p>
    </section>
    <section v-show="tab === 'controls'" id="upstream-panel-controls" role="tabpanel" aria-labelledby="upstream-tab-controls" class="upstream-panel"><AccountControlsPanel v-if="target && visible" :account="target" :snapshots="snapshots" @busy="controlsBusy = $event" @snapshot="snapshot => snapshots = { ...snapshots, [snapshot.kind]: snapshot }" @refresh-usage="tab = 'usage'; check('usage')" /></section>
    <template #footer><el-button :disabled="controlsBusy" @click="visible = false">{{ checking ? '停止等待并关闭' : '关闭' }}</el-button></template>
  </el-dialog>
</template>

<style scoped>.upstream-header { display: grid; gap: 8px; margin-bottom: 18px; overflow-wrap: anywhere; }.upstream-header strong { font-size: 17px; }.muted, .snapshot-meta { color: var(--text-3); font-size: 12px; line-height: 1.7; }.upstream-tabs { display: flex; flex-wrap: wrap; gap: 4px; border-bottom: 1px solid var(--card-border); margin-bottom: 20px; }.upstream-tabs button { flex: 1; min-width: 0; white-space: nowrap; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--text-2); padding: 11px 16px; font: inherit; font-size: 13px; cursor: pointer; }.upstream-tabs button[aria-selected="true"] { color: var(--el-color-primary); border-bottom-color: var(--el-color-primary); }.section-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }.section-heading h3, .upstream-panel > h3 { margin: 0; font-size: 14px; }.feedback { margin-bottom: 16px; }.empty-state { padding: 18px 0; color: var(--text-3); font-size: 13px; line-height: 1.7; }.upstream-facts { border: 1px solid var(--card-border); margin: 18px 0; }.upstream-facts > div { display: grid; grid-template-columns: 130px minmax(0, 1fr); border-bottom: 1px solid var(--card-border); font-size: 13px; }.upstream-facts > div:last-child { border: 0; }.upstream-facts dt { color: var(--text-3); padding: 10px; }.upstream-facts dd { margin: 0; padding: 10px; overflow-wrap: anywhere; }.quota-windows { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }.quota-window { min-width: 0; padding: 14px; border: 1px solid var(--card-border); border-radius: 5px; }.quota-title { display: flex; justify-content: space-between; gap: 8px; margin-bottom: 12px; font-size: 13px; }.quota-window progress { width: 100%; height: 7px; accent-color: var(--el-color-primary); }.quota-window p { margin: 10px 0 0; }.model-list { list-style: none; padding: 0; }.model-list li { display: flex; align-items: center; gap: 12px; border-bottom: 1px solid var(--card-border); padding: 14px 0; font-size: 13px; }.model-list li > div { display: grid; gap: 5px; min-width: 0; flex: 1; }.model-list code { font-size: 12px; color: var(--text-3); overflow-wrap: anywhere; }.full-width { width: 100%; }.inference-confirm { height: auto; white-space: normal; }.inference-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.test-prompt { margin-top: 16px; }.test-actions { display: flex; flex-wrap: wrap; gap: 8px; margin: 20px 0; }.test-actions .el-button + .el-button { margin: 0; }.test-result { border: 1px solid var(--card-border); border-radius: 5px; padding: 14px; font-size: 13px; }.stream-output pre, .test-result pre { white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12px; line-height: 1.7; background: var(--el-fill-color-light); padding: 12px; max-height: 300px; overflow-y: auto; }@media(max-width: 520px) { .quota-windows { grid-template-columns: 1fr; }.upstream-tabs button { padding: 10px 4px; font-size: 12px; }.model-list li { flex-wrap: wrap; }.model-list li .muted { display: none; }.upstream-facts > div { grid-template-columns: 110px minmax(0, 1fr); } }</style>
