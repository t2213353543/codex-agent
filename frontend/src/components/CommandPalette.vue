<script setup>
import { computed, nextTick, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { searchManagementPages } from '../utils/navigation'
const visible = defineModel({ type: Boolean, default: false })
const router = useRouter()
const search = ref('')
const active = ref(0)
const field = ref()
const results = computed(() => searchManagementPages(search.value))
const quickSearch = computed(() => search.value.trim().slice(0, 200))
const choices = computed(() => [...results.value, ...(quickSearch.value ? [{ path: '/accounts', query: { search: quickSearch.value }, title: `搜索账号：${quickSearch.value}`, description: '在账号列表中查找', icon: 'Search' }] : [])])
watch(search, () => { active.value = 0 })
watch(visible, value => { if (value) { search.value = ''; active.value = 0 } })
function move(delta) { active.value = (active.value + delta + choices.value.length) % (choices.value.length || 1); nextTick(() => document.getElementById(`command-choice-${active.value}`)?.scrollIntoView({ block: 'nearest' })) }
function select(item) { if (!item) return; visible.value = false; router.push({ path: item.path, query: item.query || {} }) }
function onKeydown(event) {
  if (event.isComposing || event.keyCode === 229) return
  if (event.key === 'ArrowDown') { event.preventDefault(); move(1) }
  else if (event.key === 'ArrowUp') { event.preventDefault(); move(-1) }
  else if (event.key === 'Enter') { event.preventDefault(); select(choices.value[active.value]) }
}
</script>
<template>
  <el-dialog v-model="visible" class="command-palette" width="620px" title="快速访问" align-center append-to-body @opened="field?.focus()">
    <el-input ref="field" v-model="search" size="large" placeholder="搜索页面或账号名称…" aria-label="搜索管理功能" role="combobox" aria-controls="command-results" aria-expanded="true" :aria-activedescendant="choices.length ? `command-choice-${active}` : undefined" @keydown="onKeydown"><template #prefix><el-icon><Search /></el-icon></template></el-input>
    <div id="command-results" class="command-results" role="listbox" aria-label="管理功能">
      <button v-for="(item, index) in choices" :id="`command-choice-${index}`" :key="`${item.path}-${index}`" class="command-choice" :class="{ active: index === active }" role="option" :aria-selected="index === active" @mouseenter="active = index" @click="select(item)">
        <span class="command-icon"><el-icon><component :is="item.icon" /></el-icon></span><span><strong>{{ item.title }}</strong><small>{{ item.description }}</small></span><span class="command-enter" aria-hidden="true">↵</span>
      </button>
    </div>
    <template #footer><div class="command-help"><span>↑ ↓ 选择　Enter 打开</span><span>Esc 关闭</span></div></template>
  </el-dialog>
</template>
<style scoped>
.command-results{display:grid;gap:4px;max-height:360px;overflow:auto;margin:14px -6px -6px;padding:4px 6px}
.command-choice{display:flex;align-items:center;gap:12px;width:100%;padding:12px;border:0;border-radius:8px;background:transparent;color:var(--text-1);text-align:left;cursor:pointer;min-width:0}
.command-choice.active{background:var(--primary-soft)}.command-choice>span:nth-child(2){min-width:0;flex:1;overflow-wrap:anywhere}.command-icon{display:grid;place-items:center;background:var(--el-fill-color-light);color:var(--text-2);width:36px;height:36px;border:1px solid var(--card-border);border-radius:9px;flex-shrink:0}
.command-choice strong{font-size:14px;font-weight:500;display:block}.command-choice small{display:block;font-size:12px;color:var(--text-3);margin-top:3px}.command-enter{color:var(--text-3);opacity:0}.command-choice.active .command-enter{opacity:1}.command-help{display:flex;justify-content:space-between;color:var(--text-3);font-size:12px}
</style>
