<script setup>
import { computed } from 'vue'
import { logSeverity } from '../utils/logSeverity'
const props = defineProps({ record: Object, explain: Boolean })
const severity = computed(() => logSeverity(props.record))
</script>
<template>
  <span v-if="severity" class="log-severity" :class="'log-severity-' + severity.tone" :title="severity.explanation" :aria-label="'错误级别：' + severity.label + '。' + severity.explanation" tabindex="0">{{ severity.label }}</span>
  <p v-if="severity && explain" class="log-severity-explanation">{{ severity.explanation }}</p>
</template>
<style scoped>
.log-severity { display:inline-flex;align-items:center;flex-shrink:0;max-width:100%;padding:1px 6px;border:1px solid currentColor;border-radius:3px;font-size:11px;line-height:1.6;font-weight:500;white-space:nowrap;cursor:help;vertical-align:middle; }.log-severity-p1 { color:var(--danger,#d9363e); }.log-severity-p2 { color:var(--warning,#a66c11); }.log-severity-p3 { color:var(--text-2); }.log-severity-unknown { color:var(--text-3); }.log-severity:focus-visible { outline:2px solid var(--primary,#409eff);outline-offset:2px; }.log-severity-explanation { color:var(--text-3);font-size:12px;line-height:1.8;margin:7px 0;overflow-wrap:anywhere; }@media(max-width:700px){.log-severity{font-size:12px}}
</style>
