<script setup>
import { onUnmounted, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { formatTime } from '../utils/format'
const props = defineProps({ account: Object })
const data = ref(null), error = ref(''), loading = ref(false)
let sequence = 0, controller
watch(() => [props.account?.id, props.account?.version], () => { data.value = null; load() }, { immediate: true })
onUnmounted(() => { sequence++; controller?.abort() })
async function load() {
  if (!props.account?.id) return
  controller?.abort(); controller = new AbortController(); const current = ++sequence
  loading.value = true; error.value = ''
  try { const result = await accountManagement.metadata(props.account.id, { signal: controller.signal }); if (current !== sequence) return; if (result.id !== props.account.id || !result.identity || !result.privacy) throw new Error('身份元数据返回格式无效'); data.value = result }
  catch (caught) { if (current === sequence && caught.name !== 'AbortError') error.value = caught.message || '读取身份元数据失败' }
  finally { if (current === sequence) loading.value = false }
}
const fields = { email: '邮箱', workspace_id: '工作区 ID', organization_id: '组织 ID', user_id: '用户 ID', plan_type: '凭据中的套餐', agent_runtime_id: 'Agent 运行时 ID', stable_key: '身份匹配键' }
</script>
<template>
  <section class="identity-panel"><div class="identity-heading"><h3>登录身份与来源</h3><el-button size="small" :loading="loading" @click="load">读取身份记录</el-button></div><p class="muted">此处读取管理侧保存的元数据，不连接上游。JWT 或导入文件中的声明未经远端验证，不能作为实时订阅或权限证明。</p><el-alert v-if="error" :title="error" type="error" :closable="false" /><p v-if="loading" role="status" class="muted">正在读取身份记录…</p><template v-if="data"><dl class="identity-fields"><div v-for="(label, key) in fields" :key="key"><dt>{{ label }}</dt><dd>{{ data.identity[key] || '未提供' }}</dd></div><div><dt>身份来源 / 记录时间</dt><dd>{{ data.identity.source || '未提供' }} · {{ formatTime(data.identity.collected_at) }}</dd></div><div><dt>训练隐私</dt><dd>{{ data.privacy.status === 'disabled' && !data.privacy.stale ? '上游已确认关闭' : '未取得当前凭据的确认' }}<span v-if="data.privacy.stale">（历史结果已陈旧）</span></dd></div><div><dt>隐私来源 / 确认时间</dt><dd>{{ data.privacy.source || '未提供' }} · {{ formatTime(data.privacy.collected_at) }}</dd></div></dl><p class="muted">实时套餐与额度请在“用量与测试”显式读取。不同工作区或不同用户属于不同身份，重新授权不会默认覆盖。</p></template></section>
</template>
<style scoped>.identity-panel { margin: 24px 0; padding-top: 18px; border-top: 1px solid var(--card-border); }.identity-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }.identity-heading h3 { font-size: 14px; margin: 0; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.identity-fields { margin: 12px 0; font-size: 12px; }.identity-fields > div { display: grid; grid-template-columns: 140px minmax(0,1fr); gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--card-border); }.identity-fields dt { color: var(--text-3); }.identity-fields dd { margin: 0; overflow-wrap: anywhere; }@media(max-width: 480px) { .identity-fields > div { grid-template-columns: 95px minmax(0,1fr); } }</style>
