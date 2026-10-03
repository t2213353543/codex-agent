<script setup>
import ConsoleIcon from './ConsoleIcon.vue'
defineProps({ items: { type: Array, required: true }, disabled: Boolean })
defineEmits(['select'])
const summaryIcon = (item, index) => item.icon || (item.action === 'enabled' ? 'check' : item.action === 'disabled' ? 'close' : ['grid', 'accounts', 'key'][index % 3])
</script>

<template>
  <div class="resource-overview resource-overview--dense" aria-label="资源概览">
    <component :is="item.action !== undefined ? 'button' : 'div'" v-for="(item, index) in items" :key="item.label" class="resource-overview-item" :class="[{ 'is-actionable': item.action !== undefined, 'is-active': item.active }, `resource-summary-tone-${item.tone || (item.action === 'enabled' ? 'green' : item.action === 'disabled' ? 'amber' : ['blue', 'purple', 'green'][index % 3])}`]" :disabled="item.action !== undefined ? disabled : undefined" :aria-pressed="item.action !== undefined ? !!item.active : undefined" :type="item.action !== undefined ? 'button' : undefined" @click="item.action !== undefined && $emit('select', item.action)">
      <span class="resource-overview-icon" aria-hidden="true"><ConsoleIcon :name="summaryIcon(item, index)" :size="16" /></span>
      <span class="resource-overview-label">{{ item.label }}</span>
      <span class="resource-overview-value">{{ item.value }}</span>
      <span class="resource-overview-hint">{{ item.hint }}</span>
    </component>
  </div>
</template>

<style scoped>
.resource-overview { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin-bottom: 18px; }
.resource-overview-item { appearance: none; display: grid; grid-template-columns: 1fr auto; align-items: center; gap: 5px 12px; min-width: 0; padding: 14px 18px; text-align: left; font: inherit; border: 1px solid var(--card-border); background: var(--card-bg); border-radius: var(--radius); box-shadow: var(--card-shadow); }
.resource-overview-label { font-size: 13px; font-weight: 500; color: var(--text-2); }
.resource-overview-value { grid-column: 2; grid-row: 1 / 3; font-size: 24px; line-height: 1.2; color: var(--text-1); font-weight: 600; font-variant-numeric: tabular-nums; }
.resource-overview-hint { font-size: 12px; line-height: 1.5; color: var(--text-3); }
.resource-overview-item.is-actionable { cursor: pointer; transition: border-color .15s, background .15s; }
.resource-overview-item.is-actionable:hover, .resource-overview-item.is-active { border-color: var(--primary); background: var(--primary-soft); }
.resource-overview-item.is-active .resource-overview-value { color: var(--primary); }
.resource-overview-item:focus-visible { outline: 2px solid var(--primary); outline-offset: 3px; }
.resource-overview-item:disabled { cursor: default; }
@media (max-width: 767px) {
  .resource-overview { gap: 8px; margin-bottom: 16px; }
  .resource-overview-item { display: flex; flex-direction: column; align-items: flex-start; padding: 12px; gap: 6px; }
  .resource-overview-label { font-size: 11px; }
  .resource-overview-value { font-size: 22px; }
  .resource-overview-hint { display: none; }
}
@container resources (max-width: 620px) {
  .resource-overview { gap: 8px; margin-bottom: 16px; }
  .resource-overview-item { display: flex; flex-direction: column; align-items: flex-start; padding: 12px; gap: 6px; }
  .resource-overview-label { font-size: 11px; }
  .resource-overview-value { font-size: 22px; }
  .resource-overview-hint { display: none; }
}
</style>

<style scoped>
.resource-overview.resource-overview--dense { display:grid; grid-template-columns:repeat(3,minmax(0,1fr)); gap:0; margin-bottom:12px; border:1px solid var(--card-border); border-radius:8px; background:var(--card-bg); overflow:hidden; }
.resource-overview.resource-overview--dense .resource-overview-item { position:relative; display:grid; grid-template-columns:30px minmax(0,1fr) auto; grid-template-rows:auto auto; align-content:center; align-items:center; gap:3px 10px; padding:11px 16px; min-height:72px; border:0; border-radius:0; background:transparent; box-shadow:none; }
.resource-overview.resource-overview--dense .resource-overview-item + .resource-overview-item { border-left:1px solid var(--card-border); }
.resource-overview-icon { display:grid; place-items:center; grid-row:1 / 3; width:30px; height:30px; border-radius:7px; color:var(--badge-blue,var(--primary)); background:var(--badge-blue-soft,var(--primary-soft)); }
.resource-summary-tone-purple .resource-overview-icon { color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); }
.resource-summary-tone-green .resource-overview-icon { color:var(--badge-green,var(--success)); background:var(--badge-green-soft,var(--primary-soft)); }
.resource-summary-tone-amber .resource-overview-icon { color:var(--badge-amber,var(--warning)); background:var(--badge-amber-soft,var(--primary-soft)); }
.resource-overview.resource-overview--dense .resource-overview-label { grid-column:2; grid-row:1; font-size:11px; line-height:1.5; color:var(--text-2); }
.resource-overview.resource-overview--dense .resource-overview-value { grid-column:3; grid-row:1 / 3; font-size:24px; line-height:1.25; font-weight:600; letter-spacing:-.6px; color:var(--text-1); }
.resource-overview.resource-overview--dense .resource-overview-hint { grid-column:2; grid-row:2; font-size:10px; line-height:1.45; color:var(--text-3); }
.resource-overview.resource-overview--dense .resource-overview-item.is-active { background:var(--primary-soft); box-shadow:inset 0 -2px 0 var(--primary); }
.resource-overview.resource-overview--dense .resource-overview-item.is-actionable:hover { background:var(--el-fill-color-light); }
.resource-overview.resource-overview--dense .resource-overview-item:focus-visible { outline-offset:-3px; }
@media(max-width:767px) {
  .resource-overview.resource-overview--dense .resource-overview-item { grid-template-columns:20px minmax(0,1fr); gap:5px; padding:10px; min-height:72px; }
  .resource-overview.resource-overview--dense .resource-overview-icon { grid-row:1; width:20px; height:20px; border-radius:4px; }
  .resource-overview.resource-overview--dense .resource-overview-icon :deep(svg) { width:12px; height:12px; }
  .resource-overview.resource-overview--dense .resource-overview-label { font-size:10px; }
  .resource-overview.resource-overview--dense .resource-overview-value { grid-column:1 / 3; grid-row:2; font-size:22px; }
  .resource-overview.resource-overview--dense .resource-overview-hint { display:none; }
}
@container resources (max-width:620px) {
  .resource-overview.resource-overview--dense .resource-overview-item { grid-template-columns:20px minmax(0,1fr); gap:5px; padding:10px; min-height:72px; }
  .resource-overview.resource-overview--dense .resource-overview-icon { grid-row:1; width:20px; height:20px; border-radius:4px; }
  .resource-overview.resource-overview--dense .resource-overview-label { font-size:10px; }
  .resource-overview.resource-overview--dense .resource-overview-value { grid-column:1 / 3; grid-row:2; font-size:22px; }
  .resource-overview.resource-overview--dense .resource-overview-hint { display:none; }
}
</style>
