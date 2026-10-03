<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { localAccountDate } from '../utils/accountManagement'
import { formatTime, parseShanghaiDateInput } from '../utils/format'
const props = defineProps({ modelValue: Boolean, account: Object })
const emit = defineEmits(['update:modelValue', 'updated', 'busy'])
const loading = ref(false), saving = ref(false), error = ref(''), info = ref(''), target = ref(null), lease = ref(null), accountVersion = ref(0)
const date = ref(''), autoDisable = ref(true), restoreOnExtend = ref(false)
const visible = computed({ get: () => props.modelValue, set: value => { if (!saving.value) emit('update:modelValue', value) } })
let generation = 0
watch(saving, value => emit('busy', value))
watch(() => props.modelValue, value => { generation++; if (value) { target.value = { ...props.account }; lease.value = null; accountVersion.value = 0; error.value = ''; info.value = ''; date.value = ''; autoDisable.value = true; restoreOnExtend.value = false; load() } })
function applyData(data) { lease.value = data.lease; accountVersion.value = data.account_version; date.value = data.lease?.expires_at ? localAccountDate(new Date(data.lease.expires_at * 1000).toISOString()) : ''; autoDisable.value = data.lease?.auto_disable ?? true; restoreOnExtend.value = data.lease?.restore_on_extend ?? false }
async function load() {
  const current = ++generation; loading.value = true; error.value = ''
  try { const data = await accountManagement.lease(target.value.id); if (current === generation && visible.value) applyData(data) }
  catch (caught) { if (current === generation) error.value = caught.message || '读取租期失败' }
  finally { if (current === generation) loading.value = false }
}
async function save() {
  if (loading.value || saving.value || !accountVersion.value) return
  const expiry = date.value ? parseShanghaiDateInput(date.value) : 0
  if (!Number.isFinite(expiry) || expiry < 0) { error.value = '请输入有效的账号到期时间'; return }
  saving.value = true; error.value = ''; info.value = ''
  try {
    await accountManagement.updateLease(target.value.id, { version: lease.value?.version || 0, account_version: accountVersion.value, expires_at: expiry, auto_disable: autoDisable.value, restore_on_extend: restoreOnExtend.value })
    await load()
    info.value = error.value ? '账号租期已保存，但最新状态读取失败，请重新读取核对' : '账号租期已保存'
    emit('updated')
  } catch (caught) { error.value = caught.status === 409 ? '账号或租期已发生变化，请重新读取后核对；当前输入已保留。' : caught.message || '保存账号租期失败' }
  finally { saving.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" title="账号到期管理" width="min(560px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!saving" :show-close="!saving" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="lease-intro"><strong>{{ target?.name }}</strong> · 账号租期与登录令牌有效期分别管理。</p><el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" /><el-alert v-if="info" :title="info" type="success" :closable="false" class="feedback" />
    <p v-if="loading" role="status">正在读取租期…</p><p class="muted">当前令牌到期：{{ formatTime(target?.expires_at) }}。刷新或重新授权不会延长这里设置的账号租期。</p>
    <el-form label-position="top" :disabled="loading || saving"><el-form-item label="账号到期时间（上海时间，留空表示永久）"><input type="datetime-local" :value="date" @input="date = $event.target.value" aria-label="账号租期到期时间" :disabled="loading || saving" class="lease-date" /></el-form-item><el-form-item><el-checkbox v-model="autoDisable">到期后自动停用账号</el-checkbox></el-form-item><el-form-item><el-checkbox v-model="restoreOnExtend" class="restore-check">延长或取消租期时，安全恢复由此租期自动停用的账号</el-checkbox></el-form-item></el-form>
    <p class="muted">自动恢复只适用于仍保持原自动停用版本的账号；手动修改或凭据变更后会继续停用，避免覆盖后续操作。</p><p v-if="lease?.disabled_by_lease">此账号由租期于 {{ formatTime(lease.disabled_at) }} 自动停用。</p><el-alert v-if="lease?.restore_skipped_reason" :title="`本次未自动恢复：${lease.restore_skipped_reason === 'account_changed' ? '账号已在自动停用后发生变化，请核对再手动启用' : lease.restore_skipped_reason}`" type="warning" :closable="false" class="feedback" />
    <template #footer><el-button :disabled="saving" @click="visible = false">关闭</el-button><el-button :disabled="saving" :loading="loading" @click="load">重新读取</el-button><el-button type="primary" :disabled="loading || !accountVersion" :loading="saving" @click="save">保存租期</el-button></template>
  </el-dialog>
</template>

<style scoped>.lease-intro { margin-top: 0; line-height: 1.7; overflow-wrap: anywhere; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.feedback { margin-bottom: 14px; }.lease-date { box-sizing: border-box; width: 100%; min-height: 34px; padding: 0 10px; color: var(--text-1); background: var(--el-fill-color-blank); border: 1px solid var(--el-border-color); border-radius: 4px; font: inherit; }.restore-check { height: auto; white-space: normal; }.restore-check :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }</style>
