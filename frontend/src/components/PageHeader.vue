<script setup>
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import ConsoleIcon from './ConsoleIcon.vue'
defineProps({
  title: { type: String, required: true },
  subtitle: { type: String, default: '' },
})
const route = useRoute()
const icon = computed(() => ({ '/': 'overview', '/accounts': 'accounts', '/groups': 'groups', '/keys': 'key', '/proxies': 'globe', '/logs': 'logs', '/settings': 'settings', '/connection': 'code', '/diagnostics': 'activity', '/audit-logs': 'logs' }[route.path] || 'overview'))
const eyebrow = computed(() => ({ '/': 'WORKSPACE OVERVIEW', '/accounts': 'ACCOUNT MANAGEMENT', '/groups': 'RESOURCE ORGANIZATION', '/keys': 'ACCESS MANAGEMENT', '/proxies': 'NETWORK CONNECTIONS', '/logs': 'TRAFFIC & OBSERVABILITY', '/settings': 'WORKSPACE PREFERENCES', '/connection': 'DEVELOPER QUICKSTART', '/diagnostics': 'SYSTEM DIAGNOSTICS', '/audit-logs': 'ACTIVITY & AUDIT' }[route.path] || 'CODEX CONSOLE'))
</script>

<template>
  <div class="page-header">
    <div class="page-header-left">
      <span class="page-heading-icon" aria-hidden="true"><ConsoleIcon :name="icon" :size="20" /></span>
      <div class="page-heading-copy"><div class="page-heading-line"><h1 class="page-header-title">{{ title }}</h1><span class="page-header-eyebrow">{{ eyebrow }}</span></div><p v-if="subtitle" class="page-header-sub">{{ subtitle }}</p></div>
    </div>
    <div class="page-header-actions">
      <slot />
    </div>
  </div>
</template>
