<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import { applyTheme, getTheme, themes } from './theme'
import { api, logoutSession } from './api'
import { readPreference, writePreference, removePreference } from './utils/preferences'
import SideNavigation from './components/SideNavigation.vue'
import ConsoleIcon from './components/ConsoleIcon.vue'
import CommandPalette from './components/CommandPalette.vue'
import './styles/console-shell.css'

const route = useRoute()
const router = useRouter()
const isLogin = computed(() => route.name === 'login' || !route.name)
const currentTheme = ref(getTheme())
const collapsed = ref(readPreference('sidebarCollapsed', false, value => typeof value === 'boolean'))
const mobileQuery = window.matchMedia('(max-width: 767px)')
const mobile = ref(mobileQuery.matches)
const navigationOpen = ref(false)
const version = ref('')
const refreshRevision = ref(0)
const logoutPending = ref(false)
const commandsOpen = ref(false)
const density = ref(readPreference('displayDensity', 'comfortable', value => ['comfortable', 'compact'].includes(value)))
const nextTheme = computed(() => themes.find(theme => theme.value !== currentTheme.value))
const currentThemeLabel = computed(() => themes.find(theme => theme.value === currentTheme.value)?.label)
let versionRequest

function setTheme(value) { currentTheme.value = applyTheme(value) }
function toggleCollapse() {
  if (mobile.value) { navigationOpen.value = !navigationOpen.value; return }
  collapsed.value = !collapsed.value
  writePreference('sidebarCollapsed', collapsed.value)
}
function onApiError(event) { ElMessage.error({ message: event.detail, grouping: true }) }
function onAuthExpired() {
  commandsOpen.value = false
  navigationOpen.value = false
  if (!isLogin.value) router.replace({ name: 'login', query: { redirect: route.fullPath } })
}
function onServerInfo(event) { version.value = String(event.detail?.version || '') }
function onThemeChanged(event) { currentTheme.value = event.detail === 'dark' ? 'dark' : 'light' }
function onViewport(event) { mobile.value = event.matches; navigationOpen.value = false }
function onShortcut(event) {
  if (!isLogin.value && (event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
    event.preventDefault()
    if (!commandsOpen.value && document.querySelector('.el-overlay-dialog .el-dialog') && [...document.querySelectorAll('.el-overlay-dialog .el-dialog')].some(el => el.getBoundingClientRect().height > 0)) return
    commandsOpen.value = !commandsOpen.value
  }
}
function refreshPage() {
  const event = new CustomEvent('page-refresh-request', { cancelable: true })
  if (!window.dispatchEvent(event)) return
  const openDialog = [...document.querySelectorAll('.el-dialog')].some(el => el.getBoundingClientRect().height > 0)
  if (openDialog) { ElMessage.info('请先完成或关闭当前弹窗，再刷新页面'); return }
  refreshRevision.value++
  readServerVersion()
}
function setDensity(value) { density.value = value; writePreference('displayDensity', value) }
function readServerVersion() {
  if (version.value || versionRequest || isLogin.value) return
  versionRequest = api.settings().catch(() => { /* The API client reports the failure. */ }).finally(() => { versionRequest = undefined })
}
async function logout() {
  if (logoutPending.value) return
  const pageState = { dirty: false, busy: false }
  window.dispatchEvent(new CustomEvent('page-unsaved-state', { detail: pageState }))
  if (pageState.busy) { ElMessage.info('正在保存，请稍后再退出'); return }
  logoutPending.value = true
  try {
    await ElMessageBox.confirm(pageState.dirty ? '当前页面有未保存的修改，退出后会丢弃这些修改。确定退出登录吗？' : '确定退出当前管理会话吗？', '退出登录', { type: 'warning', confirmButtonText: '退出', cancelButtonText: '取消' })
    await logoutSession()
    removePreference('visitedTabs')
    navigationOpen.value = false
    commandsOpen.value = false
    await router.replace('/login')
  } catch { /* Cancellation preserves the session; request failures are reported by the API client. */ }
  finally { logoutPending.value = false }
}
function onMenu(command) {
  if (command === 'logout') logout()
  else if (command.startsWith('density:')) setDensity(command.slice(8))
  else if (command === 'settings') router.push('/settings')
  else setTheme(command)
}
watch(() => route.fullPath, () => { navigationOpen.value = false })
watch(() => route.path, async () => { await nextTick(); document.querySelector('.console-content')?.scrollTo({ top: 0, left: 0 }) })
watch(isLogin, value => {
  if (value) { commandsOpen.value = false; navigationOpen.value = false }
  else readServerVersion()
})
onMounted(() => {
  window.addEventListener('api-error', onApiError)
  window.addEventListener('auth-expired', onAuthExpired)
  window.addEventListener('server-info', onServerInfo)
  window.addEventListener('theme-change', onThemeChanged)
  mobileQuery.addEventListener('change', onViewport)
  window.addEventListener('keydown', onShortcut)
  readServerVersion()
})
onUnmounted(() => {
  window.removeEventListener('api-error', onApiError)
  window.removeEventListener('auth-expired', onAuthExpired)
  window.removeEventListener('server-info', onServerInfo)
  window.removeEventListener('theme-change', onThemeChanged)
  mobileQuery.removeEventListener('change', onViewport)
  window.removeEventListener('keydown', onShortcut)
})
</script>

<template>
  <el-config-provider :locale="zhCn">
    <div v-if="isLogin" class="login-shell console-login-shell">
      <div class="console-login-themes" aria-label="页面外观">
        <button v-for="theme in themes" :key="theme.value" type="button" :class="{ active: currentTheme === theme.value }" :aria-label="`切换${theme.label}主题`" :aria-pressed="currentTheme === theme.value" @click="setTheme(theme.value)"><ConsoleIcon :name="theme.value === 'light' ? 'sun' : 'moon'" :size="15" /><span>{{ theme.label }}</span></button>
      </div>
      <router-view />
    </div>
    <div v-else class="admin-shell console-shell" :class="{ 'console-is-collapsed': collapsed }" :data-density="density" :data-console-theme="currentTheme">
      <aside v-if="!mobile" class="console-sidebar">
        <SideNavigation :collapsed="collapsed" :theme="currentTheme" :version="version" @collapse="toggleCollapse" @search="commandsOpen = true" />
      </aside>
      <el-drawer v-model="navigationOpen" class="console-mobile-navigation" title="导航" direction="ltr" size="min(308px, calc(100vw - 28px))" :with-header="false" :show-close="false">
        <SideNavigation :collapsed="false" :mobile="true" :theme="currentTheme" :version="version" @navigate="navigationOpen = false" @collapse="navigationOpen = false" @search="navigationOpen = false; commandsOpen = true" />
      </el-drawer>
      <div class="console-main-col">
        <header class="console-topbar">
          <div class="console-topbar-left">
            <button class="console-icon-button console-nav-toggle" :aria-label="mobile ? '打开导航' : collapsed ? '展开侧栏' : '收起侧栏'" :aria-expanded="mobile ? navigationOpen : !collapsed" @click="toggleCollapse"><ConsoleIcon name="menu" :size="19" /></button>
            <div class="console-breadcrumb"><router-link v-if="!mobile" to="/">工作空间</router-link><ConsoleIcon v-if="!mobile" name="chevron" :size="12" /><span>{{ route.meta.title }}</span></div>
          </div>
          <div class="console-topbar-right">
            <button class="console-command-trigger" aria-label="快速访问管理功能" @click="commandsOpen = true"><ConsoleIcon name="search" :size="17" /><span>搜索或跳转…</span><kbd>Ctrl K</kbd></button>
            <span class="console-action-divider"></span>
            <button class="console-theme-toggle" :aria-label="`切换${nextTheme.label}主题`" :title="`当前${currentThemeLabel} · 切换${nextTheme.label}`" @click="setTheme(nextTheme.value)"><ConsoleIcon :name="currentTheme === 'dark' ? 'moon' : 'sun'" :size="17" /><span>{{ currentThemeLabel }}</span></button>
            <button class="console-icon-button console-refresh" aria-label="刷新当前页面" title="刷新当前页面" @click="refreshPage"><ConsoleIcon name="refresh" :size="17" /></button>
            <el-dropdown trigger="click" @command="onMenu">
              <button class="console-user-chip" aria-label="管理员菜单"><span class="console-avatar">A</span><span class="console-user-name">管理员</span><ConsoleIcon name="chevron-down" :size="12" /></button>
              <template #dropdown><el-dropdown-menu>
                <el-dropdown-item command="settings">管理设置</el-dropdown-item>
                <el-dropdown-item v-for="theme in themes" :key="theme.value" :command="theme.value">{{ theme.label }}主题{{ currentTheme === theme.value ? ' ✓' : '' }}</el-dropdown-item>
                <el-dropdown-item divided command="density:comfortable">舒适布局{{ density === 'comfortable' ? ' ✓' : '' }}</el-dropdown-item>
                <el-dropdown-item command="density:compact">紧凑布局{{ density === 'compact' ? ' ✓' : '' }}</el-dropdown-item>
                <el-dropdown-item divided command="logout" :disabled="logoutPending">退出登录</el-dropdown-item>
              </el-dropdown-menu></template>
            </el-dropdown>
          </div>
        </header>
        <main class="content console-content"><router-view v-slot="{ Component }"><component :is="Component" :key="`${route.path}:${refreshRevision}`" /></router-view></main>
      </div>
    </div>
    <CommandPalette v-if="!isLogin" v-model="commandsOpen" />
  </el-config-provider>
</template>
