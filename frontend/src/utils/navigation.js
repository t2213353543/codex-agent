export const managementPages = [
  { path: '/', title: '仪表盘', group: '工作空间', icon: 'Odometer', description: '流量趋势、用量与最近问题', keywords: 'dashboard overview stats' },
  { path: '/accounts', title: '账号管理', group: '资源管理', icon: 'User', description: '导入账号、维护凭据和分配资源', keywords: 'account token import' },
  { path: '/groups', title: '分组管理', group: '资源管理', icon: 'Collection', description: '组织账号并关联 API 密钥', keywords: 'group' },
  { path: '/keys', title: 'API KEY 管理', group: '资源管理', icon: 'Key', description: '创建密钥、管理访问范围', keywords: 'api key access' },
  { path: '/proxies', title: 'IP 代理池', group: '资源管理', icon: 'Connection', description: '维护代理出口与账号绑定', keywords: 'proxy network' },
  { path: '/logs', title: '使用记录', group: '监控分析', icon: 'Document', description: '筛选请求、查看用量和错误', keywords: 'usage log error request' },
  { path: '/audit-logs', title: '操作记录', group: '监控分析', icon: 'Document', description: '查看已保存的管理操作', keywords: 'audit history' },
  { path: '/diagnostics', title: '运行诊断', group: '系统', icon: 'Odometer', description: '检查数据库连接和服务状态', keywords: 'health diagnostics' },
  { path: '/settings', title: '管理设置', group: '系统', icon: 'Setting', description: '管理凭据与现有服务配置', keywords: 'settings config' },
]
export function searchManagementPages(search) {
  const parts = search.trim().toLowerCase().split(/\s+/).filter(Boolean)
  return managementPages.filter(page => parts.every(part => `${page.title} ${page.description} ${page.keywords}`.toLowerCase().includes(part)))
}
