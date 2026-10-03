<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { redactImportMessage } from '../utils/importAccounts'
const props = defineProps({ resources: Object, groupMapping: Object, proxyMapping: Object, disabled: Boolean })
const emit = defineEmits(['busy', 'mapping', 'updated'])
const busy = ref(false), previewed = ref(false), executed = ref(false), uncertain = ref(false), confirmed = ref(false), error = ref(''), results = ref([])
let applyingMapping = false
const groups = computed(() => props.resources?.groups || []), proxies = computed(() => props.resources?.proxies || [])
const hasCredentials = computed(() => proxies.value.some(row => row.credentials_included === true))
const hasFailures = computed(() => results.value.some(row => row.status === 'failed'))
const states = { create: '将创建', created: '已创建', mapped: '已映射', failed: '失败' }
watch(busy, value => emit('busy', value))
watch(() => [props.resources, props.groupMapping, props.proxyMapping], () => { if (!applyingMapping) { previewed.value = false; results.value = []; error.value = '' } }, { deep: true, flush: 'sync' })
watch(confirmed, () => { previewed.value = false })
const cleanMap = value => Object.fromEntries(Object.entries(value || {}).filter(([, id]) => id))
async function run(dryRun) {
  if (busy.value || props.disabled || !dryRun && (!previewed.value || uncertain.value)) return
  if (hasCredentials.value && !confirmed.value) { error.value = '此资源备份含代理登录凭据，请明确确认导入后继续'; return }
  error.value = ''; busy.value = true
  try {
    const response = await accountManagement.importResources({ resources: props.resources, dry_run: dryRun, group_mapping: cleanMap(props.groupMapping), proxy_mapping: cleanMap(props.proxyMapping), ...(confirmed.value ? { confirm_proxy_credentials: true } : {}) })
    if (response.dry_run !== dryRun || !Array.isArray(response.results) || response.results.length !== groups.value.length + proxies.value.length || !response.group_mapping || !response.proxy_mapping) throw new Error('服务器未返回完整资源结果，请核对本地资源列表')
    results.value = response.results.map(row => ({ kind: row.kind, source_id: row.source_id, id: row.id, status: row.status, code: redactImportMessage(row.code || row.error || '', [{ payload: props.resources }]) }))
    previewed.value = dryRun; executed.value ||= !dryRun
    if (!dryRun) {
      applyingMapping = true
      emit('mapping', { groups: response.group_mapping, proxies: response.proxy_mapping })
      await nextTick()
      applyingMapping = false
      emit('updated')
    }
  } catch (caught) {
    error.value = redactImportMessage(caught.message || '资源恢复未确认', [{ payload: props.resources }]); previewed.value = false
    if (!dryRun) uncertain.value = true
  } finally { busy.value = false }
}
</script>
<template>
  <section class="resource-restore"><h4>第一步 · 恢复或映射关联资源</h4><p class="muted">文件含 {{ groups.length }} 个分组、{{ proxies.length }} 个代理。上方已选本地映射会复用对应资源，未映射项将创建；同名资源不会被覆盖。同名冲突或缺少代理密码时，请先明确映射。</p><el-checkbox v-if="hasCredentials" v-model="confirmed" :disabled="busy || disabled" class="resource-confirm">我确认导入备份中的代理用户名和密码，用于创建所选关联代理</el-checkbox><el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" /><el-alert v-if="uncertain" title="资源提交结果未确认。部分资源可能已创建，请关闭窗口、刷新本地资源后重新导入并手动映射，勿直接重复创建。" type="warning" :closable="false" class="feedback" />
    <div class="resource-toolbar"><el-button :disabled="busy || disabled || uncertain" @click="run(true)">预览资源恢复</el-button><el-button type="primary" :loading="busy" :disabled="!previewed || disabled || uncertain" @click="run(false)">确认创建或映射资源</el-button></div><p v-if="previewed" class="muted">{{ hasFailures ? '资源预览存在失败项，请先调整映射；可执行项会逐项处理。' : '资源预览完成，尚未创建资源。' }}</p><p v-if="executed" class="muted">资源阶段已执行，返回的映射已带入账号恢复。请继续下方账号预览；已创建资源不会因后续账号失败而自动撤销。</p>
    <div v-if="results.length" class="resource-results"><article v-for="row in results" :key="`${row.kind}:${row.source_id}`"><strong>{{ row.kind === 'group' ? '分组' : '代理' }} · {{ row.source_id }}</strong><span>{{ states[row.status] || row.status }}</span><p v-if="row.code">{{ row.code }}</p><p v-if="row.id" class="muted">本地 ID：{{ row.id }}</p></article></div><p class="muted">第二步在下方预览和恢复账号。资源与账号分阶段提交，可能部分成功；代理密码不会显示在预览或回执中。</p>
  </section>
</template>
<style scoped>.resource-restore { padding-top: 16px; margin-top: 20px; border-top: 1px solid var(--card-border); }.resource-restore h4 { margin: 0 0 12px; font-size: 13px; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.feedback { margin-bottom: 14px; }.resource-confirm { height: auto; white-space: normal; margin: 12px 0; }.resource-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.resource-toolbar { display: flex; flex-wrap: wrap; gap: 8px; margin: 14px 0; }.resource-toolbar .el-button + .el-button { margin: 0; }.resource-results { max-height: 260px; overflow: auto; border-block: 1px solid var(--card-border); }.resource-results article { display: grid; grid-template-columns: minmax(0,1fr) auto; gap: 6px 12px; padding: 12px 0; border-bottom: 1px solid var(--card-border); font-size: 12px; overflow-wrap: anywhere; }.resource-results p { grid-column: 1 / -1; margin: 0; }</style>
