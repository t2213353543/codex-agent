<script setup>
import { computed } from 'vue'
const props = defineProps({ items: { type: Array, required: true }, selectable: Boolean, selected: { type: Array, default: () => [] }, disabled: Boolean })
const emit = defineEmits(['select', 'detail'])
const selectedIDs = computed(() => new Set(props.selected))
const allSelected = computed(() => props.items.length > 0 && props.items.every((row) => selectedIDs.value.has(row.id)))
const someSelected = computed(() => props.items.some((row) => selectedIDs.value.has(row.id)))
function selectRow(id, value) { emit('select', value ? [...new Set([...props.selected, id])] : props.selected.filter((item) => item !== id)) }
</script>

<template>
  <div class="resource-mobile-list">
    <div v-if="selectable && items.length" class="resource-mobile-select">
      <el-checkbox :model-value="allSelected" :indeterminate="someSelected && !allSelected" :disabled="disabled" @change="$emit('select', $event ? items.map((row) => row.id) : [])">选择本页</el-checkbox>
      <span>{{ items.length }} 项</span>
    </div>
    <article v-for="row in items" :key="row.id" class="resource-mobile-card" :aria-label="row.name" :class="{ 'is-selected': selectedIDs.has(row.id) }">
      <div class="resource-mobile-card-header">
        <el-checkbox v-if="selectable" :model-value="selectedIDs.has(row.id)" :disabled="disabled" :aria-label="`选择 ${row.name}`" @change="selectRow(row.id, $event)" />
        <button class="resource-name" :disabled="disabled" @click="$emit('detail', row)">{{ row.name }}</button>
        <slot name="status" :row="row" />
      </div>
      <div class="resource-mobile-card-body"><slot :row="row" /></div>
      <div class="resource-mobile-card-actions"><slot name="actions" :row="row" /></div>
    </article>
    <slot v-if="!items.length" name="empty" />
  </div>
</template>

<style scoped>
.resource-mobile-list { display: none; }
@media (max-width: 767px) {
  .resource-mobile-list { display: block; }
  .resource-mobile-select { display: flex; align-items: center; justify-content: space-between; padding: 8px 14px; color: var(--text-3); font-size: 12px; border-bottom: 1px solid var(--card-border); }
  .resource-mobile-card { padding: 14px; border-bottom: 1px solid var(--card-border); }
  .resource-mobile-card.is-selected { background: var(--primary-soft); }
  .resource-mobile-card-header { display: flex; gap: 9px; align-items: center; }
  .resource-mobile-card-header > .resource-name { min-width: 0; flex: 1; font-size: 14px; }
  .resource-mobile-card-header > .el-checkbox { margin: 0; height: 24px; }
  .resource-mobile-card-body { margin-top: 10px; color: var(--text-2); font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
  .resource-mobile-card-actions { display: flex; justify-content: flex-end; align-items: center; gap: 8px; margin-top: 12px; padding-top: 10px; border-top: 1px solid var(--card-border); }
  .resource-mobile-card-actions :deep(.el-button) { min-height: 32px; margin: 0; }
  .resource-mobile-card-body :deep(dl) { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin: 10px 0 0; }
  .resource-mobile-card-body :deep(dt) { color: var(--text-3); }
  .resource-mobile-card-body :deep(dd) { margin: 2px 0 0; color: var(--text-1); font-variant-numeric: tabular-nums; }
}
@container resources (max-width: 620px) {
  .resource-mobile-list { display: block; }
  .resource-mobile-select { display: flex; align-items: center; justify-content: space-between; padding: 8px 14px; color: var(--text-3); font-size: 12px; border-bottom: 1px solid var(--card-border); }
  .resource-mobile-card { padding: 14px; border-bottom: 1px solid var(--card-border); }
  .resource-mobile-card.is-selected { background: var(--primary-soft); }
  .resource-mobile-card-header { display: flex; gap: 9px; align-items: center; }
  .resource-mobile-card-header > .resource-name { min-width: 0; flex: 1; font-size: 14px; }
  .resource-mobile-card-header > .el-checkbox { margin: 0; height: 24px; }
  .resource-mobile-card-body { margin-top: 10px; color: var(--text-2); font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
  .resource-mobile-card-actions { display: flex; justify-content: flex-end; align-items: center; gap: 8px; margin-top: 12px; padding-top: 10px; border-top: 1px solid var(--card-border); }
  .resource-mobile-card-actions :deep(.el-button) { min-height: 32px; margin: 0; }
  .resource-mobile-card-body :deep(dl) { display: grid; grid-template-columns: 1fr 1fr; gap: 8px; margin: 10px 0 0; }
  .resource-mobile-card-body :deep(dt) { color: var(--text-3); }
  .resource-mobile-card-body :deep(dd) { margin: 2px 0 0; color: var(--text-1); font-variant-numeric: tabular-nums; }
}
</style>
