export const defaultAccountColumns = ['name', 'status', 'concurrency', 'priority', 'weight', 'groups', 'quota', 'usage', 'latency', 'proxies', 'expires_at', 'actions']
export const accountColumnLabels = { name: '账号名称', status: '状态', concurrency: '并发', priority: '优先级', weight: '权重', groups: '分组', quota: '用量窗口', usage: '请求用量', tokens: '输入 / 输出 / 缓存读取', latency: '平均耗时', proxies: '代理', expires_at: '令牌到期', credential: '凭据状态', notes: '备注', tags: '标签', last_checked_at: '最近验证', last_used_at: '最近业务请求', created_at: '创建时间', updated_at: '更新时间', id: '账号 ID / 版本', last_error: '最近错误', actions: '操作' }

export function accountEditForm(row = {}) {
  return { name: row.name || '', enabled: row.enabled ?? true, notes: row.notes || '', tags: [...(row.tags || [])], groups: [...(row.groups || [])], proxies: [...(row.proxies || [])], concurrency: row.concurrency ?? 1, priority: row.priority ?? 0, weight: row.weight ?? 1 }
}

export function validateAccountConfiguration(form, withName = false) {
  if (withName && (!form.name.trim() || new TextEncoder().encode(form.name.trim()).length > 120)) return '账号名称不能为空，且最多 120 个 UTF-8 字节'
  for (const [key, label, min, max] of [['concurrency', '并发容量', 1, 1000], ['weight', '权重', 1, 1000], ['window_days', '窗口天数', 1, 365], ['priority', '优先级', -Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER], ['quota', 'Token 额度', 0, Number.MAX_SAFE_INTEGER]]) {
    // Legacy import defaults can still contain local accounting fields. They
    // are not part of account editing, so their absence is intentional.
    if (['quota', 'window_days'].includes(key) && !Object.hasOwn(form, key)) continue
    if (!Number.isSafeInteger(form[key]) || form[key] < min || form[key] > max) return `${label}必须为 ${min} 至 ${max} 的整数`
  }
  return ''
}

export function accountEditPatch(original, form) {
  const { reset_window, quota, window_days, ...patch } = form
  patch.name = patch.name.trim()
  patch.version = original.version
  // Account editing no longer owns local budget fields. Never send these,
  // even if an old form contains them: doing so resets accounting server-side.
  return patch
}

export function defaultImportConfiguration() {
  return { concurrency: 1, priority: 0, weight: 1, quota: 0, window_days: 30, proxies: [] }
}

export function lastUsedScopeLabel(scope) {
  return scope === 'durable_usage_events' ? '全部已持久化请求事件' : scope === 'retained_request_details' ? '仅留存请求详情' : '覆盖范围未取得'
}
