<script setup>
import { computed, ref } from 'vue'
const props = defineProps({ names: { type: Array, default: () => [] } })
const hovered = ref(false), focused = ref(false)
const text = computed(() => props.names.join(' · '))
</script>

<template>
  <el-tooltip v-if="names.length" :visible="hovered || focused" :content="text" placement="top" popper-class="account-groups-tooltip" :show-after="0">
    <span class="account-group-list" tabindex="0" :aria-label="`分组：${text}`" @mouseenter="hovered = true" @mouseleave="hovered = false" @focus="focused = true" @blur="focused = false" @keydown.esc="hovered = false; focused = false">{{ text }}</span>
  </el-tooltip>
  <span v-else class="muted">未分组</span>
</template>

<style scoped>
.account-group-list { display: block; width: fit-content; max-width: 100%; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; padding: 2px 6px; border-radius: 4px; color: var(--badge-purple,var(--primary)); background: var(--badge-purple-soft,var(--primary-soft)); font-size: 10px; line-height: 1.5; }
.account-group-list:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 2px; }
</style>
<style>
.account-groups-tooltip { max-width: min(420px, calc(100vw - 32px)); overflow-wrap: anywhere; }
</style>
