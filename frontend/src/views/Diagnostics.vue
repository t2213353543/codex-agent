<script setup>
import { formatDuration } from '../utils/format'
import { computed, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { api } from '../api'
import { useManagementRead } from '../utils/management'
import { dependencyLabels, dependencyType, diagnosticExport, downloadJSON } from '../utils/observability'
import { formatTime } from '../utils/format'
import PageHeader from '../components/PageHeader.vue'
import ConsoleIcon from '../components/ConsoleIcon.vue'
import CopyTextDialog from '../components/CopyTextDialog.vue'
import PowerPointService from '../components/PowerPointService.vue'
import OperationsMonitor from '../components/OperationsMonitor.vue'

const { loading, error, read } = useManagementRead()
const diagnostic = ref(null)
const current = ref(false)
const copyOpen = ref(false)
const copiedText = ref('')
const completedChecks = ref(0)
const healthyCount = computed(() => dependencies.value.filter(item => item.status === 'ok').length)
async function copySummary() {
  if (!current.value || loading.value) return
  const text = [`高级反代系统 ${diagnostic.value.version || ''}`, `检查时间：${formatTime(diagnostic.value.checked_at)}`, `管理服务：${diagnostic.value.status === 'ok' ? '检查通过' : '存在不可用依赖'}`, ...dependencies.value.map(item => `${item.label}：${dependencyLabels[item.status] || '未知'}${Number.isFinite(item.latency_ms) ? ` (${formatDuration(item.latency_ms)})` : ''}`)].join('\n')
  try { await navigator.clipboard.writeText(text); ElMessage.success('诊断摘要已复制') }
  catch { copiedText.value = text; copyOpen.value = true }
}
const dependencies = computed(() => ['postgres', 'redis'].map(name => ({ name, label: name === 'postgres' ? 'PostgreSQL' : 'Redis', ...diagnostic.value?.dependencies?.[name] })))
async function load() {
  current.value = false
  await read(signal => api.diagnostics({ signal, silent: true }), data => {
    if (!['ok', 'degraded'].includes(data?.status) || !['file', 'postgres'].includes(data?.storage_mode) ||
        !Number.isFinite(data.checked_at) || dependencies.value.some(item => !Object.hasOwn(dependencyLabels, data.dependencies?.[item.name]?.status))) {
      throw new Error('服务端返回了不完整的诊断结果，请重试')
    }
    diagnostic.value = data
    current.value = true
    completedChecks.value++
  })
}
function exportResult() {
  if (!current.value || loading.value) return
  try { downloadJSON(diagnosticExport(diagnostic.value), 'powerpoint-codex-diagnostics.json') }
  catch { ElMessage.error('导出失败，请重试') }
}
function uptime(value) {
  if (!Number.isFinite(value) || value < 0) return '未知'
  return `${Math.floor(value / 86400)} 天 ${Math.floor(value % 86400 / 3600)} 小时 ${Math.floor(value % 3600 / 60)} 分钟`
}
onMounted(load)
</script>
<template>
  <div class="diagnostics-page" v-loading="loading">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button :disabled="!current || loading" @click="copySummary">复制摘要</el-button><el-button :disabled="!current || loading" @click="exportResult">导出诊断结果</el-button>
      <el-button :loading="loading" aria-label="重新检查运行状态" @click="load">重新检查</el-button>
    </PageHeader>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <template v-if="diagnostic">
      <el-alert v-if="!current && !loading" title="当前检查未成功，下方保留上次结果，请重新检查。" type="warning" :closable="false" show-icon />
      <div class="diagnostic-top-grid">
        <section class="panel diagnostic-overview" aria-label="管理服务概览">
          <div class="diagnostic-summary"><h2><span class="diagnostic-icon"><ConsoleIcon name="activity" :size="15" /></span>管理服务</h2><el-tag size="small" :type="diagnostic.status === 'ok' ? 'success' : 'warning'">{{ diagnostic.status === 'ok' ? '检查通过' : '存在不可用依赖' }}</el-tag></div>
          <dl class="diagnostic-facts">
            <div><dt>后端版本</dt><dd>{{ diagnostic.version || '未知' }}</dd></div>
            <div><dt>运行时间</dt><dd>{{ uptime(diagnostic.uptime_seconds) }}</dd></div>
            <div><dt>存储方式</dt><dd>{{ diagnostic.storage_mode === 'postgres' ? 'PostgreSQL' : '文件存储' }}</dd></div>
            <div><dt>检查时间</dt><dd>{{ formatTime(diagnostic.checked_at) }}</dd></div>
          </dl>
          <div class="diagnostic-grid">
            <section v-for="item in dependencies" :key="item.name" class="diagnostic-dependency" :aria-label="`${item.label} 状态`">
              <div class="diagnostic-summary"><h3>{{ item.label }}</h3><el-tag size="small" :type="dependencyType(item.status)">{{ dependencyLabels[item.status] || '未知' }}</el-tag><span v-if="item.status === 'ok'" class="dependency-latency">检查耗时 {{ Number.isFinite(item.latency_ms) ? formatDuration(item.latency_ms) : '未知' }}</span></div>
              <p v-if="item.status === 'not_configured'">当前运行模式未配置此服务。</p>
              <p v-else-if="item.status === 'timeout'">连接检查超时。请检查服务状态与网络连接后重试。</p>
              <p v-else-if="item.status !== 'ok'">连接检查失败。请检查服务状态及连接配置后重试。</p>
            </section>
          </div>
          <div class="diagnostic-progress" role="status"><span>{{ loading ? '正在重新检查…' : current ? `已完成检查 · ${healthyCount} 个依赖连接正常` : '显示上次检查结果' }}</span><span v-if="completedChecks > 1 && current">结果已更新</span></div>
        </section>
        <PowerPointService class="panel diagnostic-inference" :inference="diagnostic.inference" compact />
      </div>
      <div class="diagnostic-note">
        <p>依赖检查验证数据库连接；运维监控读取已有请求记录与服务运行指标，不发起模型请求。</p><div class="diagnostic-links"><router-link to="/accounts">测试账号可用性 →</router-link><router-link to="/logs">查看请求错误 →</router-link></div>
        <router-link v-if="diagnostic.capabilities?.audit_logs" to="/audit-logs">查看操作记录</router-link>
        <p v-else>当前存储模式不提供持久化操作记录。</p>
      </div>
    </template>
    <el-empty v-else-if="!loading" description="暂无诊断结果，请重新检查"><el-button type="primary" @click="load">开始检查</el-button></el-empty>
    <OperationsMonitor />
    <CopyTextDialog v-model="copyOpen" :text="copiedText" label="诊断摘要" />
  </div>
</template>
<style scoped>
.diagnostic-top-grid{display:grid;grid-template-columns:minmax(0,1.12fr) minmax(0,1fr);gap:12px;align-items:stretch}
.diagnostic-top-grid .panel{padding:14px 16px;margin:0;min-width:0}
.diagnostic-summary{display:flex;align-items:center;justify-content:space-between;gap:8px;flex-wrap:wrap}
.diagnostic-summary h2{display:flex;align-items:center;gap:8px;font-size:14px;margin:0}
.diagnostic-icon{width:25px;height:25px;border-radius:6px;background:var(--badge-blue-soft);color:var(--badge-blue);display:grid;place-items:center}
.diagnostic-facts{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px 18px;margin:12px 0}
.diagnostic-facts>div{display:flex;align-items:baseline;gap:8px;min-width:0;font-size:12px;line-height:1.5}
dt{color:var(--text-3);flex-shrink:0}dd{margin:0;overflow-wrap:anywhere;font-variant-numeric:tabular-nums}
.diagnostic-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px;border-top:1px solid var(--card-border);padding-top:10px}
.diagnostic-dependency h3{font-size:12px;font-weight:600;margin:0}.diagnostic-dependency .diagnostic-summary{justify-content:flex-start;gap:6px}
.dependency-latency{font-size:11px;color:var(--text-3);white-space:nowrap}
.diagnostic-dependency p{font-size:12px;line-height:1.5;color:var(--text-2);margin:6px 0 0}
.diagnostic-progress{display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:4px 12px;margin-top:10px;font-size:11px;color:var(--text-3)}
.diagnostic-note{display:flex;align-items:baseline;flex-wrap:wrap;gap:4px 14px;margin:9px 0 0;font-size:11px;line-height:1.6;color:var(--text-3)}
.diagnostic-note p{margin:0}.diagnostic-links{display:flex;flex-wrap:wrap;gap:14px}.diagnostic-note a{color:var(--el-color-primary)}
@media(max-width:1100px){.diagnostic-top-grid{grid-template-columns:1fr}.diagnostic-facts{grid-template-columns:repeat(4,minmax(0,1fr))}.diagnostic-facts>div{display:block}.diagnostic-facts dd{margin-top:3px}}
@media(max-width:600px){.diagnostic-top-grid .panel{padding:12px}.diagnostic-facts{grid-template-columns:repeat(2,minmax(0,1fr));gap:8px 12px}.diagnostic-grid{gap:8px}.diagnostic-note{gap:4px 10px}}
@media(max-width:374px){.diagnostic-facts>div{font-size:11px}.diagnostic-grid{grid-template-columns:1fr}.diagnostic-dependency .diagnostic-summary{gap:8px}}
</style>
