<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { accountManagement } from '../api/accountManagement'
import { formatTime, timestampMillis } from '../utils/format'
import AccountAutoResetPanel from './AccountAutoResetPanel.vue'

const props = defineProps({ account: Object, snapshots: { type: Object, default: () => ({}) } })
const emit = defineEmits(['snapshot', 'busy', 'refresh-usage'])
const working = ref('')
const autoBusy = ref(false)
const error = ref('')
const selectedCredit = ref('')
const now = ref(Date.now())
const clock = setInterval(() => { now.value = Date.now() }, 1000)
onUnmounted(() => clearInterval(clock))
const result = computed(() => props.snapshots.reset_credit)
const creditsSnapshot = computed(() => props.snapshots.reset_credits)
const credits = computed(() => creditsSnapshot.value?.ok ? creditsSnapshot.value.data?.credits || [] : [])
const storagePrefix = 'codex-account-reset-operation:'
const resetMessages = { reset: '上游已确认额度重置', nothing_to_reset: '上游确认当前没有需要重置的窗口', no_credit: '上游确认无可用重置券', already_redeemed: '上游确认该券已兑换，本次未重复消费' }
watch([working, autoBusy], ([value, auto]) => emit('busy', Boolean(value) || auto))
watch(() => props.account?.id, () => { error.value = ''; selectedCredit.value = '' })
watch(credits, () => { if (!credits.value.some(credit => credit.id === selectedCredit.value)) selectedCredit.value = '' })
const expired = credit => credit.expires_at && (!Number.isFinite(timestampMillis(credit.expires_at)) || timestampMillis(credit.expires_at) <= now.value)
const canUse = credit => credit.status === 'available' && !expired(credit)
const resetDisabled = computed(() => !selectedCredit.value || !creditsSnapshot.value?.ok || creditsSnapshot.value?.stale || !creditsSnapshot.value?.checked_at || now.value - Number(creditsSnapshot.value.checked_at) * 1000 > 300000 || !(creditsSnapshot.value?.data?.available_count > 0) || !credits.value.some(credit => credit.id === selectedCredit.value && canUse(credit)) || Boolean(working.value) || autoBusy.value)
function operationID() {
  const key = `${storagePrefix}${props.account.id}:${selectedCredit.value}`
  let id
  try { id = localStorage.getItem(key) } catch { /* Memory-only retry remains available below. */ }
  if (!id || !/^[a-zA-Z0-9_-]{16,128}$/.test(id)) id = requestIDs.get(key) || crypto.randomUUID()
  requestIDs.set(key, id)
  try { localStorage.setItem(key, id) } catch { /* Optional storage. */ }
  return id
}
const requestIDs = new Map()
async function refreshCredits() {
  if (working.value || autoBusy.value) return
  working.value = 'credits'; error.value = ''
  try {
    const snapshot = await accountManagement.upstreamCheck(props.account.id, { version: props.account.version, kind: 'reset_credits' })
    if (snapshot.kind !== 'reset_credits' || snapshot.id !== props.account.id) throw new Error('服务器返回的重置券快照无效')
    emit('snapshot', snapshot)
  } catch (caught) { error.value = caught.message || '重置券查询失败' }
  finally { working.value = '' }
}
async function privacy() {
  if (working.value || autoBusy.value) return
  try { await ElMessageBox.confirm('将向上游提交关闭此账号训练使用的设置。只有收到上游确认后才显示成功。', '关闭训练使用', { confirmButtonText: '确认关闭训练', cancelButtonText: '取消' }) }
  catch { return }
  working.value = 'privacy'; error.value = ''
  try {
    const snapshot = await accountManagement.privacy(props.account.id, { version: props.account.version, disable_training: true, confirm: true })
    if (snapshot.kind !== 'privacy' || snapshot.id !== props.account.id) throw new Error('服务器返回的隐私结果无效')
    emit('snapshot', snapshot)
  } catch (caught) { error.value = caught.message || '隐私设置未确认成功' }
  finally { working.value = '' }
}
async function reset() {
  if (resetDisabled.value) return
  try { await ElMessageBox.confirm('本次操作会向上游兑换所选额度重置券，已消费的券不能退回。确认继续？', '兑换额度重置券', { type: 'warning', confirmButtonText: '确认兑换一次', cancelButtonText: '取消' }) }
  catch { return }
  working.value = 'reset'; error.value = ''
  try {
    const snapshot = await accountManagement.resetCredit(props.account.id, { version: props.account.version, credit_id: selectedCredit.value, request_id: operationID(), confirm: true })
    if (snapshot.kind !== 'reset_credit' || snapshot.id !== props.account.id) throw new Error('服务器返回的重置结果无效，请查询上游额度核对')
    emit('snapshot', snapshot)
  } catch (caught) { error.value = `兑换结果尚未确认：${caught.message || '连接中断'}。请先刷新额度核对，同一券重试将使用相同操作 ID。` }
  finally { working.value = '' }
}
</script>

<template>
  <div class="account-controls">
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <section><div class="control-heading"><h3>训练隐私</h3><el-button :loading="working === 'privacy'" :disabled="Boolean(working) || autoBusy" @click="privacy">关闭训练使用</el-button></div><p class="muted">向上游修改此账号的训练隐私设置，不改变网关转发行为。</p><template v-if="snapshots.privacy"><el-tag :type="snapshots.privacy.ok && snapshots.privacy.data?.training_allowed === false ? 'success' : 'danger'">{{ snapshots.privacy.ok && snapshots.privacy.data?.training_allowed === false ? '上游已确认关闭训练使用' : '尚未确认关闭成功' }}</el-tag><p class="muted">{{ formatTime(snapshots.privacy.checked_at) }} · {{ snapshots.privacy.source }} <span v-if="snapshots.privacy.stale">· 历史快照</span></p><p v-if="snapshots.privacy.error" class="control-error">{{ snapshots.privacy.error.message }}</p></template><p v-else class="muted">尚无本系统提交并确认的隐私设置记录。</p></section>
    <section><div class="control-heading"><h3>额度重置券</h3><el-button :loading="working === 'credits'" :disabled="Boolean(working) || autoBusy" @click="refreshCredits">查询重置券</el-button></div><p class="muted">先读取上游当前可用券，再选择具体券进行一次兑换。本地 Token 窗口重置不会消费这些券。</p>
      <template v-if="creditsSnapshot"><p class="muted">{{ formatTime(creditsSnapshot.checked_at) }} · {{ creditsSnapshot.source }} <el-tag v-if="creditsSnapshot.stale" type="warning" size="small">快照陈旧，请重新查询</el-tag></p><el-alert v-if="!creditsSnapshot.ok" :title="creditsSnapshot.error?.message || '查询重置券失败'" type="error" :closable="false" class="feedback" /><p v-else>上游可用数量：{{ creditsSnapshot.data?.available_count ?? '未知' }}</p></template><p v-else class="muted">尚未查询可用重置券。</p>
      <el-select v-if="credits.length" v-model="selectedCredit" class="full-width" aria-label="选择额度重置券" placeholder="选择一张可用重置券" :disabled="Boolean(working) || autoBusy"><el-option v-for="credit in credits" :key="credit.id" :value="credit.id" :label="`${credit.title || credit.reset_type} · ${credit.status}${expired(credit) ? ' · 已过期' : ''}`" :disabled="!canUse(credit)" /></el-select>
      <dl v-for="credit in credits.filter(item => item.id === selectedCredit)" :key="credit.id" class="credit-details"><div><dt>到期时间</dt><dd>{{ credit.expires_at ? formatTime(timestampMillis(credit.expires_at) / 1000) : '上游未提供' }}</dd></div><div v-if="credit.description"><dt>说明</dt><dd>{{ credit.description }}</dd></div></dl>
      <div class="control-actions"><el-button type="warning" :disabled="resetDisabled" :loading="working === 'reset'" @click="reset">兑换所选重置券</el-button><el-button :disabled="Boolean(working) || autoBusy" @click="emit('refresh-usage')">刷新上游额度</el-button></div>
      <div v-if="result" class="reset-result"><el-alert :title="result.data?.outcome_unknown ? '兑换结果未知，重置券可能已消费。请先刷新上游额度核对，勿换新操作重复兑换。' : result.ok ? resetMessages[result.data?.code] || '上游返回了兑换结果' : result.error?.message || '上游未确认兑换成功'" :type="result.data?.outcome_unknown ? 'warning' : result.ok ? 'info' : 'error'" :closable="false" /><p class="muted">{{ formatTime(result.checked_at) }}<span v-if="result.data?.windows_reset != null"> · 上游确认重置 {{ result.data.windows_reset }} 个窗口</span></p></div>
      <p class="muted">重试同一张券会沿用此浏览器保存的操作 ID，由服务器去重；不会自动再次消费。</p>
    </section>
    <AccountAutoResetPanel :account="account" :disabled="Boolean(working)" @busy="autoBusy = $event" />
  </div>
</template>

<style scoped>.account-controls section + section { border-top: 1px solid var(--card-border); padding-top: 22px; margin-top: 24px; }.control-heading { display: flex; gap: 12px; justify-content: space-between; align-items: center; }.control-heading h3 { font-size: 14px; margin: 0; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.control-error { color: var(--danger); font-size: 12px; overflow-wrap: anywhere; }.full-width { width: 100%; }.control-actions { display: flex; gap: 8px; flex-wrap: wrap; margin: 18px 0; }.control-actions .el-button + .el-button { margin: 0; }.credit-details { font-size: 12px; line-height: 1.7; }.credit-details > div { display: grid; grid-template-columns: 80px minmax(0,1fr); gap: 12px; margin: 8px 0; }.credit-details dt { color: var(--text-3); }.credit-details dd { margin: 0; overflow-wrap: anywhere; }.feedback { margin-bottom: 16px; }</style>
