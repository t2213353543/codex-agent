<script setup>
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { managementPages } from '../utils/navigation'
import ConsoleIcon from './ConsoleIcon.vue'

const props = defineProps({ collapsed: Boolean, mobile: Boolean, version: String, theme: { type: String, default: 'dark' } })
defineEmits(['navigate', 'collapse', 'search'])
const route = useRoute()
const dark = computed(() => props.theme === 'dark')
const groups = ['工作空间', '资源管理', '监控分析', '系统'].map(title => ({ title, items: managementPages.filter(page => page.group === title) }))
const icons = { '/': 'overview', '/accounts': 'accounts', '/groups': 'groups', '/keys': 'key', '/proxies': 'globe', '/logs': 'logs', '/audit-logs': 'audit', '/diagnostics': 'activity', '/settings': 'settings' }
const versionLabel = computed(() => props.version ? `v${props.version.replace(/^v/, '')}` : '版本待读取')
</script>

<template>
  <div class="console-navigation" :class="{ 'console-night': dark, 'console-nav-collapsed': collapsed, 'console-nav-mobile': mobile }">
    <div class="console-context">
      <div class="console-brand-row">
        <router-link to="/" class="console-brand" aria-label="高级反代系统 首页" @click="$emit('navigate')"><span class="console-brand-symbol"><ConsoleIcon :name="dark ? 'grid' : 'code'" :size="dark ? 21 : 23" /></span><span class="console-brand-text powerpoint-brand"><strong>高级反代系统</strong><small>GATEWAY · 管理控制台</small></span></router-link>
        <button v-if="mobile" class="console-icon-button console-nav-close" aria-label="关闭导航" @click="$emit('collapse')"><ConsoleIcon name="close" :size="18" /></button>
      </div>

      <nav class="console-context-links" aria-label="主要导航"><section v-for="group in groups" :key="group.title" class="console-nav-group"><h2>{{ group.title }}</h2><router-link v-for="item in group.items" :key="item.path" :to="item.path" class="console-nav-link" :class="{ active: route.path === item.path }" :aria-label="item.title" :aria-current="route.path === item.path ? 'page' : undefined" :title="collapsed ? item.title : undefined" @click="$emit('navigate')"><ConsoleIcon :name="icons[item.path]" :size="17" /><span>{{ item.title }}</span><span v-if="route.path === item.path" class="console-nav-active-dot"></span></router-link></section></nav>

      <div class="console-navigation-footer">
        <div class="console-version-row"><span class="console-version" :title="versionLabel">{{ versionLabel }}</span><button v-if="!mobile" class="console-icon-button" :aria-label="collapsed ? '展开侧栏' : '收起侧栏'" @click="$emit('collapse')"><ConsoleIcon :name="collapsed ? 'chevron' : 'chevron-left'" :size="16" /></button></div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.console-brand-text.powerpoint-brand strong{font-size:12px;letter-spacing:-.2px;white-space:normal;line-height:1.35}.console-brand-text.powerpoint-brand small{font-size:8px;letter-spacing:.4px}
</style>
