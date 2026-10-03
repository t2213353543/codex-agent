<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { logManagement } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
const props = defineProps({ modelValue: Boolean, field: String, params: Object })
const emit = defineEmits(['update:modelValue', 'select'])
const label = computed(() => ({ account: '账号', key: 'KEY', model: '模型', group: '分组' })[props.field] || '资源')
const search = ref(''), rows = ref([]), loading = ref(false), error = ref(''), more = ref(false)
const latest = createLatestRequest()
let sequence = 0, timer
async function load() {
  const id = ++sequence
  loading.value = true; error.value = ''
  try {
    await latest.run(signal => logManagement.options(props.field, search.value.trim(), props.params, { signal }), data => {
      if (!Array.isArray(data?.items) || data.items.some(row => typeof row.value !== 'string' || typeof row.label !== 'string')) throw new Error('候选响应格式无效')
      rows.value = data.items; more.value = data.has_more === true
    })
  } catch (cause) { if (id === sequence) { error.value = cause.message || '候选加载失败'; rows.value = [] } }
  finally { if (id === sequence) loading.value = false }
}
function changeSearch(value) { search.value = value; clearTimeout(timer); ++sequence; latest.cancel(); rows.value = []; loading.value = true; timer = setTimeout(load, 200) }
function searchNow() { clearTimeout(timer); load() }
function choose(row) { if (loading.value || error.value) return; emit('select', { field: props.field, ...row }); emit('update:modelValue', false) }
watch(() => props.modelValue, open => { clearTimeout(timer); if (open) { search.value = ''; rows.value = []; load() } else { ++sequence; latest.cancel() } })
onUnmounted(() => { clearTimeout(timer); ++sequence; latest.cancel() })
</script>
<template>
  <el-dialog :model-value="modelValue" :title="'查找' + label" width="min(560px, calc(100vw - 24px))" @update:model-value="emit('update:modelValue', $event)">
    <p class="lookup-note">候选来自当前已应用筛选的全部留存记录，包含已删除资源的历史标识；名称相同的资源按 ID 区分。</p>
    <el-input :model-value="search" :aria-label="'搜索' + label + '候选'" placeholder="输入名称或 ID 搜索" clearable @update:model-value="changeSearch" @keydown.enter.prevent="searchNow" />
    <p v-if="loading" class="lookup-note" role="status">正在查找匹配记录…</p>
    <div v-else-if="error" class="lookup-error" role="status">{{ error }} <el-button link type="primary" @click="load">重试查找</el-button></div>
    <div v-else class="lookup-options"><p v-if="!rows.length" class="lookup-note">当前筛选范围没有匹配候选。可取消后调整筛选，或手动输入完整值。</p><button v-for="row in rows" :key="row.value" type="button" class="lookup-option" @click="choose(row)"><span><strong>{{ row.label }}</strong><small v-if="field !== 'model'">{{ row.value }}</small></span><small v-if="row.deleted" class="lookup-deleted">已删除 · 历史记录</small><span v-else class="lookup-select">选择</span></button></div>
    <p v-if="more && !loading && !error" class="lookup-note">当前显示前 50 项，请补充搜索文本缩小范围。</p>
    <template #footer><el-button @click="emit('update:modelValue', false)">取消</el-button></template>
  </el-dialog>
</template>
<style scoped>
.lookup-note { font-size: 12px; color: var(--text-3); line-height: 1.8; margin: 0 0 14px; }.lookup-options { max-height: 360px; overflow-y: auto; margin-top: 14px; }.lookup-option { display: flex; align-items: center; gap: 12px; justify-content: space-between; width: 100%; background: transparent; padding: 11px 0; text-align: left; border: 0; border-bottom: 1px solid var(--card-border); color: var(--text-2); font: inherit; cursor: pointer; }.lookup-option > span:first-child { display: grid; gap: 5px; min-width: 0; }.lookup-option strong { font-size: 13px; font-weight: 500; overflow-wrap: anywhere; }.lookup-option small { font-size: 11px; color: var(--text-3); overflow-wrap: anywhere; }.lookup-option .lookup-deleted { flex-shrink: 0; color: var(--warning, #a66c11); font-size: 11px; }.lookup-select { color: var(--primary, #409eff); font-size: 12px; flex-shrink: 0; }.lookup-error { font-size: 12px; color: var(--danger); margin-top: 12px; }.lookup-option:focus-visible { outline: 2px solid var(--primary, #409eff); outline-offset: -2px; }
</style>
