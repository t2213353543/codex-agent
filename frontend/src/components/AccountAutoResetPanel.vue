<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { formatTime } from '../utils/format'

const props = defineProps({ account: Object, disabled: Boolean })
const emit = defineEmits(['busy'])
const loading = ref(false), saving = ref(false), error = ref(''), info = ref(''), policy = ref(null), accountVersion = ref(0)
const enabled = ref(false), threshold = ref(95), interval = ref(15), confirmConsume = ref(false), acknowledgeUnknown = ref(false)
let generation = 0, controller
const phases = { off: '已关闭', pending: '等待检查', checking: '正在读取额度', below_threshold: '尚未达到阈值', no_credit: '没有可用重置券', resetting: '正在兑换重置券', succeeded: '上次兑换成功', failed: '上次操作失败', unknown: '结果未知，已暂停', paused: '已暂停' }
const locked = computed(() => loading.value || saving.value || props.disabled)
watch(saving, value => emit('busy', value))
watch(() => props.account?.id, () => { policy.value = null; accountVersion.value = 0; error.value = ''; info.value = ''; enabled.value = false; threshold.value = 95; interval.value = 15; confirmConsume.value = false; acknowledgeUnknown.value = false; load() }, { immediate: true })
onUnmounted(() => { generation++; controller?.abort() })
function apply(data) {
  if (data.id !== props.account?.id || !Number.isInteger(data.account_version) || !data.policy || !Number.isInteger(data.policy.version)) throw new Error('自动重置策略返回无效')
  policy.value = data.policy; accountVersion.value = data.account_version; enabled.value = data.policy.enabled; threshold.value = data.policy.threshold_percent; interval.value = data.policy.interval_minutes; confirmConsume.value = false; acknowledgeUnknown.value = false
}
async function load() {
  if (!props.account?.id || saving.value) return
  controller?.abort(); controller = new AbortController(); const current = ++generation
  loading.value = true; error.value = ''
  try { const data = await accountManagement.autoReset(props.account.id, { signal: controller.signal }); if (current === generation) apply(data) }
  catch (caught) { if (current === generation && caught.name !== 'AbortError') error.value = caught.message || '自动重置策略读取失败' }
  finally { if (current === generation) loading.value = false }
}
async function save() {
  if (locked.value || !policy.value) return
  if (enabled.value && !confirmConsume.value) { error.value = '启用自动兑换前必须确认会自动消费重置券'; return }
  if (enabled.value && policy.value.phase === 'unknown' && !acknowledgeUnknown.value) { error.value = '请先核对上游额度与重置券，再明确确认解除未知结果暂停'; return }
  if (!Number.isInteger(threshold.value) || threshold.value < 1 || threshold.value > 100 || !Number.isInteger(interval.value) || interval.value < 5 || interval.value > 1440) { error.value = '阈值需为 1–100 的整数，检查间隔需为 5–1440 分钟'; return }
  saving.value = true; error.value = ''; info.value = ''
  try {
    const data = await accountManagement.updateAutoReset(props.account.id, { version: policy.value.version, account_version: accountVersion.value, enabled: enabled.value, threshold_percent: threshold.value, interval_minutes: interval.value, confirm_auto_consume: enabled.value && confirmConsume.value, acknowledge_unknown: enabled.value && acknowledgeUnknown.value })
    apply(data); info.value = data.policy.enabled ? '自动重置策略已保存；执行结果以下方状态为准' : '已关闭后续自动兑换；已经发出的兑换可能仍会完成'
  } catch (caught) { error.value = caught.status === 409 ? '账号或策略已变化，请重新读取后核对；当前输入已保留。' : caught.message || '自动重置策略保存失败' }
  finally { saving.value = false }
}
</script>

<template>
  <section class="auto-reset-panel">
    <div class="auto-reset-heading"><h3>自动额度重置</h3><el-button :disabled="saving || disabled" :loading="loading" @click="load">读取策略状态</el-button></div>
    <p class="muted">启用后，系统会定期读取真实额度。主窗口或次窗口任一达到使用阈值时，自动兑换一张有效重置券。重置券消费后不能退回。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" /><el-alert v-if="info" :title="info" type="info" :closable="false" class="feedback" />
    <el-form label-position="top" :disabled="locked || !policy"><el-form-item label="启用自动兑换"><el-switch v-model="enabled" aria-label="自动消费额度重置券" /></el-form-item><div class="auto-reset-fields"><el-form-item label="使用阈值（%）"><el-input-number v-model="threshold" :min="1" :max="100" :precision="0" aria-label="自动重置使用阈值" /></el-form-item><el-form-item label="检查间隔（分钟）"><el-input-number v-model="interval" :min="5" :max="1440" :precision="0" aria-label="自动重置检查间隔" /></el-form-item></div><el-checkbox v-if="enabled" v-model="confirmConsume" class="auto-reset-confirm">我确认此策略会自动消费上游有效重置券，消费后不能退回</el-checkbox><el-checkbox v-if="enabled && policy?.phase === 'unknown'" v-model="acknowledgeUnknown" class="auto-reset-confirm">我已核对上游额度与重置券，确认解除未知结果暂停并重新启用</el-checkbox></el-form>
    <el-button type="primary" :loading="saving" :disabled="locked || !policy" @click="save">保存自动重置策略</el-button>
    <template v-if="policy"><dl class="auto-reset-state"><div><dt>执行状态</dt><dd>{{ phases[policy.phase] || policy.phase || '未知' }}</dd></div><div><dt>上次检查</dt><dd>{{ formatTime(policy.last_checked_at) }}</dd></div><div><dt>下次检查</dt><dd>{{ policy.enabled ? formatTime(policy.next_run_at) : '已关闭' }}</dd></div><div><dt>上次兑换</dt><dd>{{ formatTime(policy.last_reset_at) }}</dd></div></dl><p v-if="policy.last_error" class="error-text">{{ policy.last_error }}</p><el-alert v-if="policy.phase === 'unknown'" title="上次兑换结果未知，重置券可能已消费。自动任务已暂停，核对上游后才能明确重新启用。" type="warning" :closable="false" /></template>
    <p class="muted">保存开关不会立即代表额度已重置。关闭只阻止后续自动任务，不会撤回已发出的兑换；此策略不改变网关转发规则。</p>
  </section>
</template>

<style scoped>.auto-reset-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }.auto-reset-heading h3 { font-size: 14px; margin: 0; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.feedback { margin-bottom: 14px; }.auto-reset-fields { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }.auto-reset-fields :deep(.el-input-number) { width: 100%; }.auto-reset-confirm { display: flex; height: auto; white-space: normal; margin: 12px 0; }.auto-reset-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.auto-reset-state { font-size: 12px; margin-top: 20px; }.auto-reset-state > div { display: grid; grid-template-columns: 90px minmax(0,1fr); gap: 12px; padding: 8px 0; border-bottom: 1px solid var(--card-border); }.auto-reset-state dt { color: var(--text-3); }.auto-reset-state dd { margin: 0; overflow-wrap: anywhere; }.error-text { color: var(--danger); overflow-wrap: anywhere; font-size: 12px; }@media(max-width: 440px) { .auto-reset-fields { grid-template-columns: 1fr; gap: 0; } }</style>
