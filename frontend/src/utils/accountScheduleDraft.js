const drafts = new Map()
const prefix = 'codex-account-schedule-draft:'
const fields = ['kind', 'model', 'cron', 'timezone', 'enabled', 'max_results', 'confirm_inference', 'request_id']

function storage() { try { return globalThis.sessionStorage } catch { return null } }
function copy(body) { return Object.fromEntries(fields.filter(key => Object.hasOwn(body, key)).map(key => [key, body[key]])) }
function valid(body) {
  return body && ['usage', 'models', 'stream', 'compact'].includes(body.kind)
    && typeof body.cron === 'string' && typeof body.timezone === 'string'
    && typeof body.enabled === 'boolean' && Number.isInteger(body.max_results)
    && body.max_results >= 1 && body.max_results <= 100
    && (body.model === undefined || typeof body.model === 'string')
    && (body.confirm_inference === undefined || typeof body.confirm_inference === 'boolean')
    && typeof body.request_id === 'string' && /^[A-Za-z0-9_-]{10,100}$/.test(body.request_id)
}

export function readScheduleDraft(accountID, store = storage()) {
  let draft = drafts.get(accountID)
  if (!draft) {
    try { draft = JSON.parse(store?.getItem(prefix + accountID) || 'null') } catch { return null }
  }
  if (!valid(draft)) return null
  drafts.set(accountID, copy(draft))
  return copy(draft)
}

export function rememberScheduleDraft(accountID, body, store = storage()) {
  if (!valid(body)) throw new Error('创建计划请求格式无效')
  const frozen = copy(body)
  drafts.set(accountID, frozen)
  try { store?.setItem(prefix + accountID, JSON.stringify(frozen)) } catch { /* Keep this page's retry identity when storage is unavailable. */ }
  return copy(frozen)
}

export function clearScheduleDraft(accountID, store = storage()) {
  drafts.delete(accountID)
  try { store?.removeItem(prefix + accountID) } catch { /* Optional session storage. */ }
}

export function definiteScheduleRejection(error) {
  return [400, 401, 403, 404, 413, 422].includes(error?.status)
}
