<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { redactImportMessage } from '../utils/importAccounts'

const props = defineProps({ modelValue: Boolean, account: Object })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const loading = ref(false)
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!loading.value) emit('update:modelValue', value) } })
const text = ref('')
const changeIdentity = ref(false)
const error = ref('')
const target = ref(null)
let readSequence = 0
watch(() => props.modelValue, (value) => { readSequence++; text.value = ''; error.value = ''; changeIdentity.value = false; target.value = value ? { ...props.account } : null })
watch(loading, (value) => emit('busy', value))
async function readFile(event) {
  const file = event.target.files?.[0]
  event.target.value = ''
  if (!file) return
  const current = ++readSequence
  if (file.size > 1024 * 1024) { error.value = '单账号凭据文件不能超过 1 MB'; return }
  try { const content = await file.text(); if (current === readSequence && visible.value && !loading.value) { text.value = content; error.value = '' } }
  catch { if (current === readSequence) error.value = '文件读取失败' }
}
async function submit() {
  if (loading.value || !target.value) return
  error.value = ''
  let credentials
  try {
    const parsed = JSON.parse(text.value)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('请提供单个账号的 JSON 对象')
    credentials = parsed.credentials ? { ...parsed.credentials, ...(parsed.agent_identity ? { agent_identity: parsed.agent_identity } : {}) } : parsed
    const token = credentials?.access_token || credentials?.tokens?.access_token || credentials?.accessToken
    if (!credentials || typeof credentials !== 'object' || Array.isArray(credentials) || typeof token !== 'string' || !token.trim()) throw new Error('凭据中缺少 access_token；请导入新授权后的完整 auth.json')
  } catch (caught) { error.value = caught instanceof SyntaxError ? '凭据 JSON 格式无效，请检查完整内容' : caught.message; return }
  loading.value = true
  try {
    await accountManagement.reauthorize(target.value.id, { version: target.value.version, credentials, allow_identity_change: changeIdentity.value })
    text.value = ''
    emit('updated')
    emit('update:modelValue', false)
  } catch (caught) { error.value = caught.status === 409 ? '账号版本或登录身份已变化。请核对身份变更选项；版本冲突需关闭窗口后刷新账号再试。' : redactImportMessage(caught.message || '重新授权失败', [{ payload: { credentials } }]) }
  finally { loading.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" title="重新授权账号" width="min(640px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!loading" :show-close="!loading" @closed="text = ''" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="reauthorize-intro">替换 <strong>{{ target?.name || target?.id }}</strong> 的登录凭据，保留账号 ID、分组、代理、配置与历史记录。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <label class="credential-upload">选择新的 auth.json <input type="file" accept=".json,application/json" :disabled="loading" @change="readFile" /></label>
    <el-input v-model="text" type="textarea" :rows="8" aria-label="重新授权凭据 JSON" placeholder="粘贴新授权后的 auth.json 或包含 access_token 的 credentials 对象" :disabled="loading" autocomplete="off" />
    <el-checkbox v-model="changeIdentity" :disabled="loading" class="identity-confirm">我确认允许更换此账号对应的上游登录身份</el-checkbox>
    <p class="muted">默认只接受同一身份。刷新令牌不会从旧身份自动沿用；关闭后清除输入的凭据。</p>
    <template #footer><el-button :disabled="loading" @click="visible = false">取消</el-button><el-button type="primary" :loading="loading" :disabled="!text.trim()" @click="submit">替换凭据</el-button></template>
  </el-dialog>
</template>

<style scoped>.reauthorize-intro { margin-top: 0; line-height: 1.7; overflow-wrap: anywhere; }.credential-upload { display: flex; flex-wrap: wrap; gap: 8px; margin: 16px 0; font-size: 13px; }.credential-upload input { min-width: 0; max-width: 100%; }.identity-confirm { margin-top: 16px; height: auto; white-space: normal; }.identity-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.6; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.feedback { margin-bottom: 14px; }</style>
