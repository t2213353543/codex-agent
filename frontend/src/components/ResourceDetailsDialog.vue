<script setup>
defineProps({ modelValue: Boolean, title: String, kind: String, fields: { type: Array, default: () => [] }, metrics: { type: Array, default: () => [] } })
defineEmits(['update:modelValue'])
</script>

<template>
  <el-dialog :model-value="modelValue" :title="`${kind}详情`" width="min(680px, calc(100vw - 24px))" class="detail-dialog resource-detail-dialog" align-center append-to-body @update:model-value="$emit('update:modelValue', $event)">
    <div class="resource-detail-head"><span class="resource-detail-kind">{{ kind }}</span><h3>{{ title }}</h3><slot name="status" /></div>
    <div v-if="metrics.length" class="resource-detail-metrics" :style="{ gridTemplateColumns: `repeat(${metrics.length}, minmax(0, 1fr))` }"><div v-for="metric in metrics" :key="metric.label"><strong>{{ metric.value }}</strong><span>{{ metric.label }}</span></div></div>
    <dl class="resource-detail-fields"><div v-for="field in fields" :key="field.label"><dt>{{ field.label }}</dt><dd :class="{ mono: field.mono }">{{ field.value ?? '—' }}</dd></div></dl>
    <div class="resource-detail-section"><slot /></div>
    <template #footer><div class="resource-detail-actions"><el-button @click="$emit('update:modelValue', false)">关闭</el-button><slot name="actions" /></div></template>
  </el-dialog>
</template>

<style scoped>
.resource-detail-head { padding: 0 0 18px; border-bottom: 1px solid var(--card-border); }
.resource-detail-kind { display: block; margin-bottom: 8px; color: var(--text-3); font-size: 11px; }
.resource-detail-head h3 { margin: 0 0 10px; font-size: 20px; font-weight: 600; color: var(--text-1); overflow-wrap: anywhere; }
.resource-detail-metrics { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; margin: 18px 0; padding: 14px 12px; background: var(--el-fill-color-light); border: 1px solid var(--card-border); border-radius: 8px; }
.resource-detail-metrics > div { display: flex; flex-direction: column; gap: 5px; text-align: center; }
.resource-detail-metrics strong { color: var(--text-1); font-weight: 600; font-size: 22px; }
.resource-detail-metrics span { color: var(--text-3); font-size: 11px; }
.resource-detail-fields { margin: 22px 0; }
.resource-detail-fields > div { display: grid; grid-template-columns: 86px minmax(0, 1fr); gap: 12px; margin-bottom: 16px; font-size: 13px; line-height: 1.7; }
.resource-detail-fields dt { color: var(--text-3); }
.resource-detail-fields dd { margin: 0; color: var(--text-1); overflow-wrap: anywhere; }
.resource-detail-section { border-top: 1px solid var(--card-border); padding-top: 20px; color: var(--text-2); font-size: 13px; line-height: 1.7; }
.resource-detail-actions { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.resource-detail-actions .el-button + .el-button { margin-left: 0; }
.resource-detail-section > :first-child { margin-top: 0; }
.resource-detail-section > :last-child { margin-bottom: 0; }
@media (max-width: 480px) {
  .resource-detail-fields > div { grid-template-columns: 76px minmax(0, 1fr); gap: 8px; }
  .resource-detail-metrics strong { font-size: 20px; }
  .resource-detail-actions { gap: 6px; }
  .resource-detail-actions :deep(.el-button) { flex: 1 1 auto; }
}
</style>
