<script setup>
import { computed } from 'vue'
import { formatTime } from '../utils/format'
import { officialUsageWindows, accountWindowCostLabel, accountWindowCostHint, accountWindowTotalLabel, accountWindowTotalHint } from '../utils/accountOfficialUsage'

const props = defineProps({ snapshot: Object, loading: Boolean, error: String })
defineEmits(['open'])
const windows = computed(() => officialUsageWindows(props.snapshot))
const percent = value => value === null ? '未知' : `${Number(value.toFixed(1))}%`
const updated = computed(() => props.snapshot?.checked_at ? `官方查询于 ${formatTime(props.snapshot.checked_at)}` : '官方用量尚未取得')
const reset = window => window.reset_at ? formatTime(window.reset_at) : '未知'
const total = window => accountWindowTotalLabel(window).replace('总额度估算 ', '总约 ')
</script>

<template>
  <div class="official-usage-windows" :title="updated">
    <div v-if="windows.length" class="official-usage-list">
      <el-popover v-for="window in windows" :key="window.key" trigger="click" placement="bottom" :width="320" :persistent="false" :popper-style="{ maxWidth: 'calc(100vw - 24px)' }">
        <template #reference>
          <button type="button" class="official-usage-window" :aria-label="`查看官方 ${window.label} 用量与重置时间`">
            <span class="official-window-meter">
              <span class="official-usage-caption"><span>{{ window.label }}</span><strong :class="{ 'is-full': window.used_percent >= 100 }">{{ percent(window.used_percent) }}</strong></span>
              <progress v-if="window.progress !== null" :value="window.progress" max="100" :aria-label="`官方 ${window.label} 用量`" :class="{ 'is-full': window.used_percent >= 100 }" />
            </span>
            <span class="official-usage-amounts">
              <span class="official-usage-cost" :title="accountWindowCostHint(window.local_cost)">{{ accountWindowCostLabel(window.local_cost) }}</span>
              <span v-if="total(window)" class="official-usage-total" :title="accountWindowTotalHint(window)">{{ total(window) }}</span>
            </span>
          </button>
        </template>
        <div class="official-window-detail">
          <div class="official-detail-heading"><strong>{{ window.label }} · 已用 {{ percent(window.used_percent) }}</strong><span v-if="snapshot?.stale">历史快照</span></div>
          <dl><div><dt>重置时间</dt><dd>{{ reset(window) }}</dd></div><div><dt>窗口费用</dt><dd>{{ accountWindowCostLabel(window.local_cost) }}</dd></div><div v-if="accountWindowTotalLabel(window)"><dt>总额度估算</dt><dd>{{ accountWindowTotalLabel(window).replace('总额度估算 ', '') }}</dd></div></dl>
          <p>{{ accountWindowCostHint(window.local_cost) }}</p>
          <p v-if="accountWindowTotalLabel(window)">{{ accountWindowTotalHint(window) }}</p>
          <div class="official-detail-footer"><span>{{ updated }}</span><button type="button" @click="$emit('open')">官方额度详情</button></div>
        </div>
      </el-popover>
    </div>
    <span v-else class="official-usage-empty">{{ loading ? '查询官方用量…' : error ? '官方用量未取得' : snapshot?.ok ? '官方未提供窗口' : '尚未查询' }}</span>
    <div class="official-usage-controls">
      <span v-if="error" class="official-usage-error" :title="error">查询失败</span><span v-else-if="loading">更新中</span><span v-else-if="snapshot?.stale">历史</span>
      <button type="button" aria-label="官方额度详情" :title="updated" @click="$emit('open')">详情</button>
    </div>
  </div>
</template>

<style scoped>
.official-usage-windows { display: flex; align-items: center; gap: 8px; padding: 0; font-size: 10px; line-height: 1.4; min-width: 0; font-variant-numeric: tabular-nums; }
.official-usage-list { display: grid; flex: 1; gap: 5px; min-width: 0; }
.official-usage-window { display: grid; grid-template-columns: 70px minmax(0, 1fr); align-items: center; gap: 8px; width: 100%; padding: 2px 0; border: 0; background: transparent; font: inherit; line-height: 1.4; text-align: left; color: var(--text-2); cursor: pointer; }
.official-usage-window:hover { color: var(--el-color-primary); }
.official-usage-window:focus-visible, .official-usage-controls button:focus-visible, .official-detail-footer button:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 2px; border-radius: 3px; }
.official-window-meter { display: grid; gap: 3px; min-width: 0; }
.official-usage-caption { display: flex; align-items: baseline; justify-content: space-between; gap: 5px; }
.official-usage-caption strong { font-weight: 600; color: var(--text-1); white-space: nowrap; }
.official-usage-caption strong.is-full { color: var(--el-color-danger); }
.official-usage-amounts { display: flex; flex-wrap: wrap; align-items: center; gap: 2px 7px; min-width: 0; }
.official-usage-cost, .official-usage-total { overflow-wrap: anywhere; }
.official-usage-total { color: var(--text-3); }
.official-usage-window progress { display: block; width: 100%; height: 3px; border: 0; border-radius: 3px; overflow: hidden; appearance: none; background: var(--el-fill-color); accent-color: var(--el-color-primary); }
.official-usage-window progress::-webkit-progress-bar { background: var(--el-fill-color); }
.official-usage-window progress::-webkit-progress-value { background: var(--el-color-primary); }
.official-usage-window progress.is-full::-webkit-progress-value { background: var(--el-color-danger); }
.official-usage-empty, .official-usage-controls { color: var(--text-3); }
.official-usage-empty { flex: 1; }
.official-usage-controls { display: grid; flex: none; gap: 2px; justify-items: end; font-size: 10px; }
.official-usage-controls button, .official-detail-footer button { padding: 2px 0; border: 0; background: transparent; color: var(--el-color-primary); font: inherit; cursor: pointer; }
.official-usage-error { color: var(--el-color-danger); }
.official-window-detail { font-size: 12px; line-height: 1.6; color: var(--text-2); overflow-wrap: anywhere; }
.official-detail-heading { display: flex; align-items: center; justify-content: space-between; gap: 8px; }
.official-detail-heading strong { color: var(--text-1); }
.official-detail-heading > span { color: var(--el-color-warning); font-size: 11px; }
.official-window-detail dl { display: grid; gap: 5px; margin: 10px 0; font-variant-numeric: tabular-nums; }
.official-window-detail dl > div { display: grid; grid-template-columns: 70px minmax(0, 1fr); gap: 8px; }
.official-window-detail dt { color: var(--text-3); }
.official-window-detail dd { margin: 0; }
.official-window-detail p { margin: 7px 0; color: var(--text-3); font-size: 11px; }
.official-detail-footer { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 4px; margin-top: 9px; border-top: 1px solid var(--card-border); padding-top: 7px; color: var(--text-3); font-size: 10px; }
@media(max-width: 767px) { .official-usage-windows { font-size: 11px; }.official-usage-window { grid-template-columns: 74px minmax(0, 1fr); gap: 8px; min-height: 30px; }.official-usage-controls button { min-height: 32px; padding: 6px 0; } }
</style>
