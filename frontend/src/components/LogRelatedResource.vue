<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { logManagement } from '../api/logManagement'
import { relatedLogResource } from '../utils/logRelatedResource'
import { createLatestRequest } from '../utils/latestRequest'
import { logTokenCount } from '../utils/logQueryState'
import { formatTime } from '../utils/format'
const props = defineProps({ modelValue: Boolean, resource: Object })
const emit = defineEmits(['update:modelValue', 'filter', 'manage'])
const label = computed(() => props.resource?.kind === 'key' ? 'KEY' : '分组')
const loading = ref(false), error = ref(''), item = ref(null), loaded = ref(false)
const latest = createLatestRequest()
let sequence = 0
async function load() {
  const id = ++sequence
  loading.value = true; error.value = ''; loaded.value = false; item.value = null
  if (!props.resource?.id) { loading.value = false; loaded.value = true; return }
  const requested = { ...props.resource }
  try { await latest.run(signal => logManagement.related(requested.kind, { signal }), data => { item.value = relatedLogResource(data, requested.kind, requested.id); loaded.value = true }) }
  catch (cause) { if (id === sequence) error.value = cause.message || '资源加载失败' }
  finally { if (id === sequence) loading.value = false }
}
function filter() { emit('filter', props.resource.kind === 'key' ? 'key_id' : 'group_id', props.resource.id); emit('update:modelValue', false) }
watch(() => props.modelValue, open => { if (open) load(); else { ++sequence; latest.cancel() } })
onUnmounted(() => { ++sequence; latest.cancel() })
</script>
<template>
  <el-dialog :model-value="modelValue" :title="label + ' 关联资源'" width="min(560px, calc(100vw - 24px))" @update:model-value="emit('update:modelValue', $event)">
    <p class="related-note">按请求中记录的稳定 ID 查询当前资源。当前名称、分组与统计可能已变化，历史请求不会随之改写。</p>
    <dl class="related-fields"><dt>资源 ID</dt><dd>{{ resource?.id || '历史记录未保存 ID' }}</dd><dt>请求时名称</dt><dd>{{ resource?.name || '历史记录未保存名称' }}</dd></dl>
    <p v-if="loading" role="status" class="related-note">正在读取关联资源…</p>
    <div v-else-if="error" class="related-error" role="status">{{ error }} <el-button link type="primary" @click="load">重试关联资源</el-button></div>
    <p v-else-if="loaded && !item" class="related-missing" role="status">{{ resource?.id ? '该资源已删除或当前不可用。保留历史标识，不会按同名资源自动替换。' : '无法唯一定位资源。历史名称仅作为快照保留。' }}</p>
    <dl v-else-if="item" class="related-fields related-current"><dt>当前名称</dt><dd>{{ item.name || '未命名' }}</dd><template v-if="resource.kind === 'key'"><dt>当前状态</dt><dd>{{ item.enabled === true ? '启用' : item.enabled === false ? '停用' : '未知' }}</dd><dt>当前分组</dt><dd>{{ (item.group_names?.length ? item.group_names : item.groups || []).join('、') || '未分组' }}</dd><dt>累计请求</dt><dd>{{ logTokenCount(item.requests) }}</dd><dt>累计 Token</dt><dd>{{ logTokenCount(item.tokens) }}</dd><dt>最近使用</dt><dd>{{ item.last_used_at ? formatTime(item.last_used_at) : '未记录' }}</dd></template><template v-else><dt>说明</dt><dd>{{ item.description || '—' }}</dd><dt>当前账号关联</dt><dd>{{ logTokenCount(item.accounts) }}</dd><dt>当前 KEY 关联</dt><dd>{{ logTokenCount(item.keys) }}</dd></template></dl>
    <p v-if="item" class="related-note">管理页展示完整资源列表；此处已按 ID 精确定位。累计用量口径由资源接口提供，不等同于本次日志筛选。</p>
    <template #footer><el-button @click="emit('update:modelValue', false)">关闭关联资源</el-button><el-button v-if="resource?.id" @click="filter">筛选该{{ label }}使用记录</el-button><el-button v-if="item" type="primary" @click="emit('manage', resource.kind); emit('update:modelValue', false)">进入{{ label }}管理</el-button></template>
  </el-dialog>
</template>
<style scoped>
.related-note { color: var(--text-3); font-size: 12px; line-height: 1.8; margin: 0 0 15px; }.related-fields { display: grid; grid-template-columns: 105px minmax(0, 1fr); gap: 12px; font-size: 12px; margin: 0 0 18px; }.related-fields dt { color: var(--text-3); }.related-fields dd { margin: 0; color: var(--text-2); overflow-wrap: anywhere; }.related-current { border-top: 1px solid var(--card-border); padding-top: 18px; }.related-error, .related-missing { font-size: 12px; line-height: 1.8; color: var(--warning, #a66c11); background: var(--el-fill-color-light); padding: 12px; margin: 0 0 15px; }@media(max-width:500px) { .related-fields { grid-template-columns: 86px minmax(0, 1fr); } }
</style>
