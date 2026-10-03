<script setup>
import { formatTime } from '../utils/format'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { lookupIPGeo } from '../api/geoLookup.js'
import { geoIPState, geoLocationLabel, readGeoEntry } from '../utils/geoLookupState.js'

const props = defineProps({ ip: { type: String, default: '' } })
const entry = ref(null)
const loading = ref(false)
const error = ref('')
const details = ref(false)
const classification = computed(() => geoIPState(props.ip))
let controller
let generation = 0
function reset() {
  generation++
  controller?.abort()
  controller = null
  entry.value = null
  loading.value = false
  error.value = ''
  details.value = false
}
watch(() => props.ip, reset)
onBeforeUnmount(reset)
async function lookup(force = false) {
  if (loading.value || classification.value.status !== 'idle') return
  const ip = classification.value.ip
  const current = ++generation
  controller?.abort()
  controller = new AbortController()
  loading.value = true
  error.value = ''
  entry.value = null
  try {
    const payload = await lookupIPGeo([ip], { force, signal: controller.signal })
    if (generation !== current) return
    const result = readGeoEntry(payload, ip)
    entry.value = result
    if (result.status === 'error') error.value = result.error || 'GeoJS 查询失败，请稍后手动重试'
    details.value = result.status === 'success'
  } catch (failure) {
    if (generation === current && failure?.name !== 'AbortError') error.value = failure?.message || 'GeoJS 查询失败，请稍后手动重试'
  } finally {
    if (generation === current) loading.value = false
  }
}
function timestamp(value) { return formatTime(value) }
</script>

<template>
  <section class="log-ip-geo" aria-label="IP 归属地">
    <p v-if="!ip" class="geo-muted">未记录 IP，无法查询归属地</p>
    <p v-else-if="classification.status === 'invalid'" class="geo-muted">IP 格式无效，仅支持完整 IPv4 / IPv6 地址</p>
    <p v-else-if="classification.status === 'private' || entry?.status === 'private'" class="geo-muted">私有、保留或非公网地址，不发送至 GeoJS</p>
    <template v-else>
      <p class="geo-disclosure">点击后将此公网 IP 发送给 GeoJS，仅用于查询大致归属地。</p>
      <p v-if="loading" class="geo-muted" role="status">正在通过 GeoJS 查询…</p>
      <p v-if="error" class="geo-error" role="alert">{{ error }}</p>
      <template v-if="entry?.status === 'success'">
        <button type="button" class="geo-link geo-result" :aria-expanded="details" @click="details = !details">{{ geoLocationLabel(entry) }} · {{ details ? '收起详情' : '查看详情' }}</button>
        <dl v-if="details" class="geo-details">
          <div><dt>国家 / 地区</dt><dd>{{ entry.country || entry.country_code }}</dd></div>
          <div><dt>省 / 州</dt><dd>{{ entry.region || '未提供' }}</dd></div>
          <div><dt>城市</dt><dd>{{ entry.city || '未提供' }}</dd></div>
          <div><dt>所属组织</dt><dd>{{ entry.organization || '未提供' }}</dd></div>
          <div><dt>时区</dt><dd>{{ entry.timezone || '未提供' }}</dd></div>
        </dl>
        <p class="geo-meta">来源：<a href="https://www.geojs.io/docs/v1/endpoints/geo/" target="_blank" rel="noopener noreferrer">GeoJS</a> · 查询于 {{ timestamp(entry.fetched_at) }}{{ entry.cached ? ' · 使用 24 小时内缓存' : '' }}</p>
        <p class="geo-muted">IP 归属地为网络地址估计，不代表用户实时或精确位置。</p>
      </template>
      <button type="button" class="geo-link" :disabled="loading" @click="lookup(entry?.status === 'success' || !!error)">{{ entry?.status === 'success' ? '重新通过 GeoJS 查询' : error ? '重试 GeoJS 查询归属地' : '通过 GeoJS 查询归属地' }}</button>
    </template>
  </section>
</template>

<style scoped>
.log-ip-geo{min-width:0;margin-top:8px;line-height:1.6;font-size:12px;overflow-wrap:anywhere}.log-ip-geo p{margin:4px 0}.geo-muted,.geo-disclosure,.geo-meta{color:var(--text-3,#64748b)}.geo-error{color:var(--danger,#dc2626)}.geo-link{border:0;background:none;padding:2px 0;color:var(--primary,#2563eb);font:inherit;text-align:left;text-decoration:underline;text-underline-offset:3px;cursor:pointer}.geo-link:disabled{opacity:.6;cursor:wait}.geo-link:focus-visible{outline:2px solid var(--primary,#2563eb);outline-offset:3px}.geo-result{font-weight:600}.geo-details{margin:8px 0;display:grid;gap:5px}.geo-details>div{display:grid;grid-template-columns:80px minmax(0,1fr);gap:8px}.geo-details dt{color:var(--text-3,#64748b)}.geo-details dd{margin:0}.geo-meta a{color:inherit}
</style>
