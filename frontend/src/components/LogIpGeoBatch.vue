<script setup>
import { formatTime } from '../utils/format'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { lookupIPGeo } from '../api/geoLookup.js'
import { geoLocationLabel, planGeoBatch, runGeoBatch } from '../utils/geoLookupState.js'

const props = defineProps({ ips: { type: Array, default: () => [] }, disabled: { type: Boolean, default: false } })
const visible = ref(false)
const loading = ref(false)
const error = ref('')
const canceled = ref(false)
const entries = ref([])
const targets = ref([])
let controller
let generation = 0
const plan = computed(() => {
  try { return { ...planGeoBatch(props.ips), error: '' } }
  catch (failure) { return { ips: [], error: failure.message } }
})
const successes = computed(() => entries.value.filter(item => item.status === 'success').length)
function stop() { controller?.abort() }
function reset() {
  generation++
  stop()
  loading.value = false
  visible.value = false
  entries.value = []
  targets.value = []
  error.value = ''
  canceled.value = false
}
watch(() => JSON.stringify(props.ips), reset)
watch(() => props.disabled, value => { if (value) reset() })
onBeforeUnmount(reset)
function open() {
  if (props.disabled || loading.value) return
  entries.value = []
  error.value = ''
  canceled.value = false
  targets.value = [...plan.value.ips]
  visible.value = true
}
async function lookup(force = false) {
  if (loading.value || props.disabled || !targets.value.length) return
  const current = ++generation
  controller = new AbortController()
  loading.value = true
  entries.value = []
  error.value = ''
  canceled.value = false
  try {
    await runGeoBatch(targets.value, lookupIPGeo, {
      signal: controller.signal, force,
      onBatch: batch => { if (generation === current) entries.value.push(...batch) },
    })
  } catch (failure) {
    if (generation !== current) return
    if (failure?.name === 'AbortError') canceled.value = true
    else error.value = failure?.message || 'GeoJS 批量查询失败'
  } finally { if (generation === current) loading.value = false }
}
function close() { stop(); visible.value = false }
function timestamp(value) { return formatTime(value) }
</script>

<template>
  <el-button :disabled="disabled || loading || !ips.length" @click="open">批量查询本页 IP 归属地</el-button>
  <el-dialog v-model="visible" title="本页 IP 归属地" width="min(720px, calc(100vw - 24px))" append-to-body :close-on-click-modal="false" @close="stop">
    <div class="geo-batch-body">
      <p>本次仅查询当前已显示页面的 IP，去重后有 <strong>{{ targets.length }}</strong> 个可查询公网地址。点击“通过 GeoJS 查询”才会将这些 IP 发送给 GeoJS，每批最多 50 个，不发送请求内容、账号或密钥。</p>
      <p v-if="plan.private_count || plan.invalid_count || plan.missing_count || plan.duplicate_count" class="geo-batch-muted">已排除私有 / 保留地址 {{ plan.private_count }} 个、格式无效 {{ plan.invalid_count }} 条、未记录 {{ plan.missing_count }} 条；合并重复地址 {{ plan.duplicate_count }} 个。</p>
      <p v-if="plan.error" class="geo-batch-error" role="alert">{{ plan.error }}</p>
      <p v-if="!targets.length && !plan.error" class="geo-batch-muted">本页没有可查询的公网 IP。</p>
      <p v-if="error" class="geo-batch-error" role="alert">{{ error }}</p>
      <p v-if="loading || entries.length || canceled" role="status">已完成 {{ entries.length }} / {{ targets.length }} 个，成功 {{ successes }} 个，未成功 {{ entries.length - successes }} 个。{{ canceled ? '已取消，未完成部分没有结果。' : loading ? '正在查询…' : '' }}</p>
      <div v-if="entries.length" class="geo-batch-results" aria-label="本页 IP 归属地查询结果">
        <article v-for="entry in entries" :key="entry.ip" class="geo-batch-row">
          <div class="geo-batch-row-head"><code>{{ entry.ip }}</code><span>{{ entry.status === 'success' ? '查询成功' : entry.status === 'private' ? '未外发' : '查询失败' }}</span></div>
          <template v-if="entry.status === 'success'">
            <p>{{ geoLocationLabel(entry) }}</p><p class="geo-batch-muted">所属组织：{{ entry.organization || '未提供' }} · 时区：{{ entry.timezone || '未提供' }}</p>
            <p class="geo-batch-muted">来源：GeoJS · 查询于 {{ timestamp(entry.fetched_at) }}{{ entry.cached ? ' · 服务端缓存' : '' }}</p>
          </template>
          <p v-else class="geo-batch-error">{{ entry.error || '未能取得归属地，请手动重试' }}</p>
        </article>
      </div>
      <p class="geo-batch-muted">归属地来自 <a href="https://www.geojs.io/docs/v1/endpoints/geo/" target="_blank" rel="noopener noreferrer">GeoJS</a>，仅表示网络地址的大致位置。成功结果在服务端内存缓存 24 小时，重启后失效。</p>
    </div>
    <template #footer><div class="geo-batch-actions"><el-button @click="close">关闭</el-button><el-button v-if="loading" @click="stop">取消未完成查询</el-button><template v-else><el-button v-if="entries.length" :disabled="disabled || !targets.length" @click="lookup(true)">重新通过 GeoJS 查询</el-button><el-button v-else type="primary" :disabled="disabled || !targets.length" @click="lookup(false)">通过 GeoJS 查询 {{ targets.length }} 个公网 IP</el-button></template></div></template>
  </el-dialog>
</template>

<style scoped>
.geo-batch-body{font-size:13px;line-height:1.7;min-width:0;overflow-wrap:anywhere}.geo-batch-body>p{margin:0 0 12px}.geo-batch-muted{color:var(--text-3,#64748b);font-size:12px}.geo-batch-error{color:var(--danger,#dc2626)}.geo-batch-results{max-height:45vh;overflow:auto;border:1px solid var(--card-border,#dbe3ef);border-radius:8px;margin-bottom:12px}.geo-batch-row{padding:12px;border-bottom:1px solid var(--card-border,#dbe3ef)}.geo-batch-row:last-child{border-bottom:0}.geo-batch-row p{margin:4px 0}.geo-batch-row-head{display:flex;gap:12px;justify-content:space-between;flex-wrap:wrap}.geo-batch-row-head code{font-size:12px;overflow-wrap:anywhere}.geo-batch-actions{display:flex;gap:8px;flex-wrap:wrap;justify-content:flex-end}.geo-batch-actions :deep(.el-button+.el-button){margin-left:0}.geo-batch-body a{color:var(--primary,#2563eb)}
</style>
