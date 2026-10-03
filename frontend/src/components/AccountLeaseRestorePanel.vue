<script setup>
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { leaseBackup } from '../api/accountLeaseBackup.js'
import { leaseRestoreItems, leaseRestoreReason, leaseRestoreStatus, mergeLeaseRestoreResults, validLeaseRestore } from '../utils/accountLeaseBackup.js'
import { formatTime } from '../utils/format'

const props = defineProps({ receipt: Object, disabled: Boolean })
const emit = defineEmits(['busy', 'updated'])
const current = ref(null), busy = ref(false), error = ref(''), uncertain = ref(false), previewed = ref(false)
const confirm = ref(false), confirmExpired = ref(false), confirmReplace = ref(false), canceled = ref(false)
let generation = 0, controller, stopRequested = false
function stopLaterBatches() { stopRequested = true }
watch(busy, value => emit('busy', value), { flush: 'sync' })
function reset() { generation++; controller?.abort(); busy.value = false; current.value = props.receipt ? JSON.parse(JSON.stringify(props.receipt)) : null; previewed.value = false; uncertain.value = false; error.value = ''; confirm.value = false; confirmExpired.value = false; confirmReplace.value = false; canceled.value = false }
watch(() => props.receipt, reset, { immediate: true })
onBeforeUnmount(() => { generation++; controller?.abort() })
const rows = computed(() => current.value?.items || [])
const pending = computed(() => rows.value.filter(item => item.status === 'pending'))
const needsExpired = computed(() => pending.value.some(item => item.auto_disable && item.expires_at > 0 && item.expires_at <= Date.now() / 1000))
const needsReplace = computed(() => pending.value.some(item => item.replaces_existing))
const hasUnknown = computed(() => rows.value.some(item => ['running', 'unknown'].includes(item.status)))
const canApply = computed(() => previewed.value && confirm.value && (!needsExpired.value || confirmExpired.value) && (!needsReplace.value || confirmReplace.value) && !uncertain.value && pending.value.length > 0)
async function refresh() {
  if (busy.value || !current.value?.receipt_id) return
  const id = ++generation; busy.value = true; error.value = ''; controller = new AbortController()
  try {
    const result = await leaseBackup.receipt(current.value.receipt_id, { signal: controller.signal })
    if (generation !== id) return
    if (!validLeaseRestore(result, rows.value.length) || result.receipt_id !== current.value.receipt_id) throw new Error('服务器租期回执不完整')
    current.value = result; uncertain.value = false; previewed.value = false
  } catch (failure) { if (generation === id && failure.name !== 'AbortError') error.value = failure.status === 409 ? '租期回执已过期，请重新导入备份并核对账号后再恢复租期。' : failure.message || '读取回执失败' }
  finally { if (generation === id) busy.value = false }
}
async function run(dryRun) {
  if (busy.value || props.disabled || !current.value?.receipt_id || !dryRun && !canApply.value) return
  const id = ++generation; const targets = leaseRestoreItems(current.value)
  busy.value = true; error.value = ''; canceled.value = false; stopRequested = false; controller = new AbortController()
  try {
    for (let offset = 0; offset < targets.length; offset += 100) {
      if (stopRequested) { canceled.value = true; break }
      const items = targets.slice(offset, offset + 100)
      const result = await leaseBackup.restore({ receipt_id: current.value.receipt_id, items, dry_run: dryRun, ...(!dryRun ? { confirm_restore: confirm.value, confirm_expired: confirmExpired.value, confirm_replace: confirmReplace.value } : {}) }, { signal: controller.signal })
      if (generation !== id) return
      if (result.dry_run !== dryRun) throw new Error('租期恢复阶段不匹配')
      current.value = mergeLeaseRestoreResults(current.value, result, items)
      if (!dryRun) emit('updated')
    }
    previewed.value = dryRun && !canceled.value
  } catch (failure) {
    if (generation !== id) return
    previewed.value = false
    error.value = failure.status === 409 ? '回执或目标版本已变化，请读取回执并重新核对。' : failure.message || '租期恢复失败'
    if (!dryRun) uncertain.value = true
  } finally { if (generation === id) busy.value = false }
}
</script>

<template>
  <section class="lease-restore" aria-label="备份租期恢复">
    <h4>独立恢复租期</h4>
    <p class="lease-note">备份包含 {{ rows.length }} 项租期配置。账号恢复完成后，按返回的新账号 ID 与版本核对，再独立确认租期；不会复制原实例的停用所有权，也不会因此启用已停用账号。</p>
    <p v-if="current?.error" class="lease-error" role="alert">账号阶段已执行，但租期回执无法保存。请重新导入并核对重复账号，租期尚未恢复。</p>
    <p v-if="error" class="lease-error" role="alert">{{ error }}</p>
    <p v-if="uncertain || hasUnknown" class="lease-warning" role="alert">部分租期结果尚未确认。读取回执并核对目标账号租期，不要重复执行未知项。</p>
    <p v-if="canceled" class="lease-note">已停止后续批次，已完成的租期保留。</p>
    <div class="lease-rows">
      <article v-for="item in rows" :key="item.account_index">
        <div class="lease-row-title"><strong>{{ item.account_index }} · {{ item.name }}</strong><span>{{ leaseRestoreStatus[item.status] }}</span></div>
        <p v-if="item.id" class="lease-note">目标 ID：{{ item.id }} · 账号版本 {{ item.account_version }} · 租期版本 {{ item.lease_version }}</p>
        <p>租期：{{ item.expires_at ? formatTime(item.expires_at) : '不限期' }} · 到期自动停用：{{ item.auto_disable ? '是' : '否' }} · 延期恢复策略：{{ item.restore_on_extend ? '启用' : '关闭' }}</p>
        <p v-if="item.expired" class="lease-warning">此租期已过期{{ item.auto_disable ? '，恢复时启用中的账号会立即停用' : '，未开启自动停用' }}。</p>
        <p v-if="item.replaces_existing" class="lease-warning">目标已有租期配置，将在双版本校验通过后覆盖。</p>
        <p v-if="item.code" class="lease-error">{{ leaseRestoreReason[item.code] || '请核对该项恢复结果' }}</p>
      </article>
    </div>
    <template v-if="current?.receipt_id">
      <p class="lease-note">回执有效至 {{ formatTime(current.expires_at) }}。账号和租期分阶段提交，租期失败不会撤销已经完成的账号导入。</p>
      <div class="lease-confirmations"><el-checkbox v-model="confirm" :disabled="busy || disabled">我确认将备份租期应用到以上固定 ID 的账号</el-checkbox><el-checkbox v-if="needsReplace" v-model="confirmReplace" :disabled="busy || disabled">我确认覆盖这些账号已有的本地租期配置</el-checkbox><el-checkbox v-if="needsExpired" v-model="confirmExpired" :disabled="busy || disabled">我确认已过期且开启自动停用的租期可能立即停用账号</el-checkbox></div>
      <div class="lease-actions"><el-button :disabled="busy || disabled" @click="refresh">读取租期恢复回执</el-button><el-button :disabled="busy || disabled || uncertain || !pending.length" @click="run(true)">预览租期恢复</el-button><el-button type="primary" :loading="busy" :disabled="disabled || !canApply" @click="run(false)">确认恢复租期</el-button><el-button v-if="busy" @click="stopLaterBatches">停止后续批次</el-button></div>
      <p v-if="previewed" class="lease-note">租期预览完成，尚未修改租期。待恢复 {{ pending.length }} 项。</p>
    </template>
    <p v-else class="lease-note">先完成账号恢复，再预览并确认租期。账号预览不会写入租期。</p>
  </section>
</template>

<style scoped>
.lease-restore{border-top:1px solid var(--card-border);margin-top:22px;padding-top:16px;min-width:0;font-size:13px;line-height:1.7}.lease-restore h4{margin:0 0 10px;font-size:14px}.lease-note{font-size:12px;color:var(--text-3)}.lease-error{color:var(--danger);font-size:12px}.lease-warning{color:var(--warning);font-size:12px}.lease-rows{max-height:340px;overflow:auto}.lease-rows article{padding:12px 0;border-bottom:1px solid var(--card-border);overflow-wrap:anywhere}.lease-rows p{margin:5px 0}.lease-row-title{display:flex;gap:12px;justify-content:space-between;flex-wrap:wrap}.lease-confirmations{display:grid;gap:9px;margin:14px 0}.lease-confirmations .el-checkbox{height:auto;white-space:normal;align-items:flex-start}.lease-confirmations :deep(.el-checkbox__label){white-space:normal;line-height:1.7}.lease-actions{display:flex;flex-wrap:wrap;gap:8px}.lease-actions :deep(.el-button+.el-button){margin-left:0}
</style>
