<script setup>
import { computed } from 'vue'
import { logModelConsistency } from '../utils/logModelConsistency'

const props = defineProps({ record: { type: Object, default: () => ({}) } })
const comparison = computed(() => logModelConsistency(props.record))
</script>

<template>
  <el-tooltip placement="top" :show-after="150" :trigger="['hover', 'focus']">
    <template #content><div class="model-consistency-tooltip">{{ comparison.tooltip }}</div></template>
    <span class="model-consistency" :class="`model-consistency-${comparison.status}`" :data-model-consistency="comparison.status" tabindex="0" :aria-label="`${comparison.label}。${comparison.tooltip}`"><i aria-hidden="true" />{{ comparison.label }}</span>
  </el-tooltip>
</template>

<style scoped>
.model-consistency { display: inline-flex; align-items: center; gap: 4px; max-width: 100%; font-size: 10px; line-height: 1.6; color: var(--text-3); cursor: help; white-space: nowrap; }
.model-consistency i { width: 5px; height: 5px; border-radius: 50%; background: currentColor; flex-shrink: 0; }
.model-consistency-match { color: var(--success, #529b2e); }
.model-consistency-mismatch { color: var(--warning, #b88230); }
.model-consistency:focus-visible { outline: 2px solid var(--primary); outline-offset: 3px; border-radius: 2px; }
.model-consistency-tooltip { max-width: min(460px, 80vw); white-space: pre-line; overflow-wrap: anywhere; line-height: 1.7; }
</style>
