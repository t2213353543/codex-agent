export function parseBatchReauthorization(source, targets) {
  let parsed
  try { parsed = JSON.parse(source) } catch { throw new Error('凭据映射必须是完整 JSON，解析错误详情不会回显敏感内容') }
  const rows = Array.isArray(parsed) ? parsed : parsed?.items
  if (!Array.isArray(rows) || !rows.length || rows.length > 10000) throw new Error('请提供 1–10000 条凭据映射数组，或含 items 数组的对象')
  const byID = new Map(targets.map(row => [row.id, row])), seen = new Set()
  return rows.map((row, index) => {
    if (!row || typeof row.id !== 'string' || !byID.has(row.id)) throw new Error(`第 ${index + 1} 项 ID 不在已选账号快照中`)
    if (seen.has(row.id)) throw new Error(`第 ${index + 1} 项重复指定了同一账号 ID`)
    seen.add(row.id)
    const target = byID.get(row.id)
    if (row.version !== undefined && row.version !== target.version) throw new Error(`第 ${index + 1} 项版本与选择快照不一致，请重新核对`)
    const credentials = row.credentials
    if (!credentials || typeof credentials !== 'object' || Array.isArray(credentials)) throw new Error(`第 ${index + 1} 项需要独立 credentials 对象`)
    const token = credentials.access_token || credentials.tokens?.access_token || credentials.accessToken
    if (typeof token !== 'string' || !token.trim() || /\s/.test(token) || new TextEncoder().encode(token).length > 128 * 1024) throw new Error(`第 ${index + 1} 项缺少有效 access_token`)
    return { id: target.id, version: target.version, name: target.name || target.id, credentials, allow_identity_change: row.allow_identity_change === true, state: 'pending', message: '' }
  })
}
