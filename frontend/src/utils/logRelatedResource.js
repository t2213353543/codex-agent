// Only displayable resource metadata crosses this boundary. Never retain a
// complete KEY value, token or arbitrary backend property in the dialog state.
export function relatedLogResource(data, kind, id) {
  const rows = data?.[kind === 'key' ? 'keys' : 'groups']
  if (!Array.isArray(rows)) throw new Error('资源响应格式无效，请重试')
  const row = rows.find(value => value?.id === id)
  if (!row) return null
  const allowed = kind === 'key' ? ['id', 'name', 'enabled', 'groups', 'group_names', 'created_at', 'last_used_at', 'requests', 'tokens', 'errors'] : ['id', 'name', 'description', 'accounts', 'keys', 'created_at']
  return Object.fromEntries(allowed.filter(key => row[key] !== undefined).map(key => [key, Array.isArray(row[key]) ? [...row[key]] : row[key]]))
}
