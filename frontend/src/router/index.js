import { createRouter, createWebHashHistory } from 'vue-router'
import { ensureSession } from '../api'

const routes = [
  { path: '/redirect', redirect: '/' },
  { path: '/login', name: 'login', component: () => import('../views/Login.vue'), meta: { title: '登录' } },
  {
    path: '/', name: 'dashboard', component: () => import('../views/Dashboard.vue'),
    meta: { title: '仪表盘', group: '总览', icon: 'Odometer', subtitle: '网关运行状态与流量总览' },
  },
  {
    path: '/groups', name: 'groups', component: () => import('../views/Groups.vue'),
    meta: { title: '分组管理', group: '资源管理', icon: 'Collection', subtitle: '创建分组，账号可归属一个或多个分组' },
  },
  {
    path: '/keys', name: 'keys', component: () => import('../views/Keys.vue'),
    meta: { title: 'API KEY 管理', group: '资源管理', icon: 'Key', subtitle: '为分组签发网关密钥，控制访问范围' },
  },
  {
    path: '/proxies', name: 'proxies', component: () => import('../views/Proxies.vue'),
    meta: { title: 'IP 代理池', group: '资源管理', icon: 'Connection', subtitle: '管理代理节点，账号可绑定多个代理出口' },
  },
  {
    path: '/accounts', name: 'accounts', component: () => import('../views/Accounts.vue'),
    meta: { title: '账号管理', group: '资源管理', icon: 'User', subtitle: '导入与维护账号、分组和代理绑定' },
  },
  {
    path: '/logs', name: 'logs', component: () => import('../views/Logs.vue'),
    meta: { title: '使用记录', group: '监控', icon: 'Document', subtitle: '每一次网关转发的请求明细' },
  },
  {
    path: '/diagnostics', name: 'diagnostics', component: () => import('../views/Diagnostics.vue'),
    meta: { title: '运行诊断', group: '系统', icon: 'Odometer', subtitle: '检查管理服务及数据库连接状态' },
  },
  {
    path: '/audit-logs', name: 'audit-logs', component: () => import('../views/AuditLogs.vue'),
    meta: { title: '操作记录', group: '监控', icon: 'Document', subtitle: '查询已持久化的管理操作记录' },
  },
  {
    path: '/settings', name: 'settings', component: () => import('../views/Settings.vue'),
    meta: { title: '管理设置', group: '系统', icon: 'Setting', subtitle: '管理登录凭据与查看服务信息' },
  },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

const router = createRouter({
  history: createWebHashHistory(),
  routes,
})

router.beforeEach(async (to) => {
  document.title = '高级反代系统'
  const hasKey = await ensureSession()
  if (to.name !== 'login' && !hasKey) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && hasKey) {
    return { name: 'dashboard' }
  }
})

export default router
