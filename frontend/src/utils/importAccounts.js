// Import parsing is deliberately strict: malformed JSON is never interpreted as a token.
const credentialKeys = ['access_token', 'refresh_token', 'id_token', 'account_id', 'chatgpt_account_id', 'client_id']
export const maxImportBytes = 32 * 1024 * 1024
const maxImportAccounts = 10000
const limitRows = (rows, line = 1) => rows.length > maxImportAccounts ? [failure(line, '每次最多导入 10000 个账号，请分批处理')] : rows
const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

function failure(line, message) {
  return { line, kind: 'failed', name: '—', message, action: 'skip' }
}

function jsonErrorLine(error, text, firstLine = 1) {
  const position = /position\s+(\d+)/i.exec(error.message)
  if (position) return firstLine + text.slice(0, Number(position[1])).split('\n').length - 1
  const line = /line\s+(\d+)/i.exec(error.message)
  return line ? firstLine + Number(line[1]) - 1 : firstLine + text.slice(0, locateJsonError(text)).split('\n').length - 1
}

// Newer JS engines sometimes omit the parser offset. Locate syntax errors without
// echoing source fragments (which may contain a credential) into the UI.
function locateJsonError(text) {
  let position = 0
  const whitespace = () => { while (/\s/.test(text[position] || '') && position < text.length) position++ }
  const invalid = () => { throw position }
  function string() {
    const start = position++
    let escaped = false
    while (position < text.length) {
      const char = text[position++]
      if (escaped) { escaped = false; continue }
      if (char === '\\') { escaped = true; continue }
      if (char === '"') {
        try { JSON.parse(text.slice(start, position)); return } catch { position = start; invalid() }
      }
      if (char.charCodeAt(0) < 32) { position--; invalid() }
    }
    invalid()
  }
  function value(depth = 0) {
    whitespace()
    if (depth > 256) invalid()
    const char = text[position]
    if (char === '"') return string()
    if (char === '{' || char === '[') {
      const object = char === '{'
      const closing = object ? '}' : ']'
      position++; whitespace()
      if (text[position] === closing) { position++; return }
      while (position < text.length) {
        whitespace()
        if (object) {
          if (text[position] !== '"') invalid()
          string(); whitespace()
          if (text[position++] !== ':') { position--; invalid() }
        }
        value(depth + 1); whitespace()
        if (text[position] === closing) { position++; return }
        if (text[position++] !== ',') { position--; invalid() }
      }
      invalid()
    }
    const match = /^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/.exec(text.slice(position))
    if (!match) invalid()
    position += match[0].length
  }
  try { value(); whitespace(); if (position < text.length) invalid() }
  catch (offset) { return typeof offset === 'number' ? offset : position }
  return position
}

function arrayItemLines(text) {
  const lines = []
  let depth = 0
  let quoted = false
  let escaped = false
  let line = 1
  let awaiting = false
  for (const char of text) {
    if (char === '\n') line++
    if (quoted) {
      if (escaped) escaped = false
      else if (char === '\\') escaped = true
      else if (char === '"') quoted = false
      continue
    }
    if (depth === 1 && awaiting && !/\s/.test(char) && char !== ']') {
      lines.push(line)
      awaiting = false
    }
    if (char === '"') quoted = true
    else if (char === '[' || char === '{') {
      depth++
      if (depth === 1) awaiting = true
    } else if (char === ']' || char === '}') depth--
    else if (char === ',' && depth === 1) awaiting = true
  }
  return lines
}

function normalize(value, line) {
  if (!isObject(value)) return failure(line, '每条账号必须是 JSON 对象')
  let credentials = value
  for (let depth = 0; depth < 8; depth++) {
    if (!isObject(credentials)) return failure(line, 'tokens / credentials 必须是 JSON 对象')
    if (Object.hasOwn(credentials, 'tokens')) credentials = credentials.tokens
    else if (Object.hasOwn(credentials, 'credentials')) credentials = credentials.credentials
    else break
  }
  if (!isObject(credentials)) return failure(line, 'tokens / credentials 必须是 JSON 对象')
  if (Object.hasOwn(credentials, 'tokens') || Object.hasOwn(credentials, 'credentials')) return failure(line, 'tokens / credentials 嵌套层级不能超过 8 层')
  // Keep account metadata and unknown credential fields intact. The server is
  // responsible for interpreting supported fields; the preview and commit must
  // receive the same original configuration.
  const clean = { ...credentials }
  for (const key of credentialKeys) {
    if (!Object.hasOwn(credentials, key)) continue
    if (typeof credentials[key] !== 'string') return failure(line, `${key} 必须是字符串`)
    clean[key] = credentials[key].trim()
  }
  if (!clean.access_token) return failure(line, '缺少非空 access_token')
  if (/\s/.test(clean.access_token)) return failure(line, 'access_token 不能包含空白字符')
  if (new TextEncoder().encode(clean.access_token).length > 128 * 1024) return failure(line, 'access_token 不能超过 128 KB')
  const expiry = Object.hasOwn(credentials, 'expires_at') ? credentials.expires_at : Object.hasOwn(value, 'platform') ? undefined : value.expires_at
  if (expiry !== undefined) {
    const numeric = typeof expiry === 'number' || (typeof expiry === 'string' && /^-?\d+$/.test(expiry))
    const validDate = typeof expiry === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(expiry) && Number.isFinite(Date.parse(expiry))
    if (!(numeric && Number.isSafeInteger(Number(expiry)) && Number(expiry) >= 0) && !validDate) return failure(line, 'expires_at 必须是非负整数时间戳或 RFC3339 时间')
    clean.expires_at = expiry
  }
  if (value.name !== undefined && typeof value.name !== 'string') return failure(line, 'name 必须是字符串')
  const name = value.name?.trim() || ''
  if (name && (new TextEncoder().encode(name).length > 100 || /[\x00-\x1f\x7f-\x9f\u2028\u2029]/u.test(name))) {
    return failure(line, '名称不能超过 100 字节或包含控制字符')
  }
  const payload = { ...value, credentials: clean }
  delete payload.tokens
  if (name) payload.name = name
  return { line, name, credentials: clean, payload, rawSource: value, generatedName: !name, externalBindings: Boolean(value.proxy_key || value.proxy_id || value.group_ids?.length || value.groups?.length || value.proxies?.length) }
}

function documentRows(parsed, text, firstLine) {
  if (isObject(parsed) && Object.hasOwn(parsed, 'accounts')) {
    if (parsed.type !== undefined && parsed.type !== '' && !['sub2api-data', 'sub2api-bundle'].includes(parsed.type)) return [failure(firstLine, '不支持的导出文件类型，请使用 Sub2API 账号导出文件')]
    if (parsed.version !== undefined && ![0, 1].includes(parsed.version)) return [failure(firstLine, '不支持的导出文件版本，目前支持版本 1')]
    if (!Array.isArray(parsed.accounts)) return [failure(firstLine, 'accounts 必须是账号数组')]
    if (Object.hasOwn(parsed, 'proxies') && !Array.isArray(parsed.proxies)) return [failure(firstLine, 'proxies 必须是代理数组')]
    const match = /"accounts"\s*:\s*\[/.exec(text)
    const offset = match ? match.index + match[0].length - 1 : 0
    const precedingLines = text.slice(0, offset).split('\n').length - 1
    const lines = arrayItemLines(text.slice(offset))
    if (!parsed.accounts.length) return [failure(firstLine, '导出文件中没有账号')]
    return parsed.accounts.map((value, index) => ({ ...normalize(value, (lines[index] || 1) + precedingLines), format: 'Sub2API', externalBindings: Boolean(parsed.proxies?.length || value?.proxy_key || value?.proxy_id || value?.group_ids?.length || value?.groups?.length || value?.proxies?.length) }))
  }
  if (Array.isArray(parsed)) {
    if (!parsed.length) return [failure(firstLine, '账号数组不能为空')]
    const lines = arrayItemLines(text)
    return parsed.map((item, index) => normalize(item, lines[index] || firstLine))
  }
  return [normalize(parsed, firstLine)]
}

export function parseImportText(source) {
  const text = String(source || '').replace(/^\uFEFF/, '')
  const trimmed = text.trim()
  if (!trimmed) return []
  const firstLine = text.slice(0, text.indexOf(trimmed[0])).split('\n').length
  if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
    try {
      const parsed = JSON.parse(text)
      const rows = documentRows(parsed, text, firstLine)
      return limitRows(rows, firstLine)
    } catch (error) {
      const nonempty = text.split(/\r?\n/).map((raw, index) => ({ raw: raw.trim(), line: index + 1 })).filter(({ raw }) => raw)
      // JSONL is identified by complete object lines, never by the opening brace alone.
      const jsonl = !trimmed.startsWith('[') && nonempty.length > 1 && nonempty.every(({ raw }) => raw.startsWith('{')) && nonempty.some(({ raw }) => {
        try { return isObject(JSON.parse(raw)) } catch { return false }
      })
      if (!jsonl) return [failure(jsonErrorLine(error, text), 'JSON 格式错误；请检查此行附近的引号、逗号或括号')]
      return limitRows(nonempty.map(({ raw, line }) => {
        try { return normalize(JSON.parse(raw), line) }
        catch { return failure(line, 'JSONL 格式错误；该行必须是完整 JSON 对象') }
      }))
    }
  }
  return limitRows(text.split(/\r?\n/).flatMap((raw, index) => {
    const source = raw.trim()
    if (!source) return []
    const separator = source.indexOf('----')
    const name = separator >= 0 ? source.slice(0, separator).trim() : undefined
    const token = separator >= 0 ? source.slice(separator + 4).trim() : source
    if (separator >= 0 && (!name || !token)) return [failure(index + 1, '别名格式为「名称----access_token」，两侧均不能为空')]
    if (/^[\[{"\]}]/.test(token) || /[\s|,，:]/.test(token) || !/^[A-Za-z0-9._~+/=-]+$/.test(token)) {
      return [failure(index + 1, '每行填写一个 token 或「名称----token」；JSON 不得与 token 混合')]
    }
    if (['null', 'true', 'false', 'undefined'].includes(token) || /^\d+$/.test(token)) return [failure(index + 1, '此行不是有效的账号对象或 token')]
    return [normalize({ name, access_token: token }, index + 1)]
  }))
}

export function parseImportDocuments(documents) {
  const rows = documents.flatMap(({ name, text }) => parseImportText(text).map((row) => ({ ...row, sourceFile: name || '' })))
  if (rows.length > maxImportAccounts) return [failure(1, '每次最多导入 10000 个账号，请分批处理')]
  return rows.map((row, sourceIndex) => ({ ...row, sourceIndex }))
}

// The raw adapter needs outer Sub2API fields independently of token metadata.
// Keep each original row attached to its file/line; only an explicit previewed
// name is added at commit time. Never rebuild the request from credentials.
export function importRequestSource(row, name) {
  const source = row.rawSource || row.payload
  if (!isObject(source)) throw new Error('导入项目缺少原始账号对象，请重新预览')
  return name === undefined ? source : { ...source, name }
}

export async function tokenFingerprint(token) {
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器无法安全生成账号标识，请使用 HTTPS 或本机地址访问')
  const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(token))
  return Array.from(new Uint8Array(bytes), (byte) => byte.toString(16).padStart(2, '0')).join('')
}

export async function prepareImport(source, existingAccounts = []) {
  const rows = parseImportText(source)
  const existing = new Set(existingAccounts.map((account) => account.name))
  const seenTokens = new Map()
  const seenNames = new Map()
  for (const row of rows) {
    if (row.kind === 'failed') continue
    row.fingerprint = await tokenFingerprint(row.credentials.access_token)
    if (row.generatedName) row.name = `acct-${row.fingerprint}`
    if (seenTokens.has(row.fingerprint)) {
      row.kind = 'duplicate'
      row.message = `与第 ${seenTokens.get(row.fingerprint)} 行 token 相同，自动跳过`
      row.action = 'skip'
      row.locked = true
      continue
    }
    if (seenNames.has(row.name)) {
      row.kind = 'failed'
      row.message = `与第 ${seenNames.get(row.name)} 行名称相同但 token 不同，请修改名称后重新预览`
      row.action = 'skip'
      continue
    }
    seenTokens.set(row.fingerprint, row.line)
    seenNames.set(row.name, row.line)
    if (existing.has(row.name)) {
      row.kind = row.generatedName ? 'duplicate' : 'replace'
      row.message = row.generatedName ? '已有相同 token 标识；默认跳过，可明确选择更新凭据' : '同名账号已存在；只有明确选择替换后才覆盖凭据'
      row.action = 'skip'
    } else {
      row.kind = 'new'
      row.message = '新名称；将新增账号'
      row.action = 'add'
    }
  }
  return rows
}

export function redactImportMessage(message, rows = []) {
  let text = String(message || '')
  const secrets = new Set(), seen = new WeakSet()
  const secretKeys = new Set(['access_token', 'refresh_token', 'id_token', 'client_id', 'accessToken', 'refreshToken', 'password', 'agent_private_key', 'private_key', 'session_token', 'sessionToken'])
  const collect = (value, depth = 0) => {
    if (!value || typeof value !== 'object' || depth > 32 || seen.has(value)) return
    seen.add(value)
    for (const [key, child] of Object.entries(value)) {
      if (secretKeys.has(key) && typeof child === 'string' && child) secrets.add(child)
      else if (child && typeof child === 'object') collect(child, depth + 1)
    }
  }
  for (const row of rows) collect(row)
  for (const secret of [...secrets].sort((left, right) => right.length - left.length)) text = text.split(secret).join('[REDACTED]')
  return text.replace(/Bearer\s+\S+/gi, 'Bearer [REDACTED]').replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, '[REDACTED]')
}

export function importReport(rows, results = []) {
  const byIndex = new Map(results.filter((result) => result.sourceIndex !== undefined).map((result) => [result.sourceIndex, result]))
  const byLine = new Map(results.filter((result) => result.sourceIndex === undefined).map((result) => [result.line, result]))
  return rows.map((row) => {
    const result = byIndex.get(row.sourceIndex) || byLine.get(row.line)
    return {
      line: row.line,
      item: row.sourceIndex === undefined ? undefined : row.sourceIndex + 1,
      name: row.name,
      classification: row.kind,
      action: row.action,
      result: result?.status || (row.kind === 'failed' ? 'failed' : row.action === 'skip' ? 'skipped' : 'pending'),
      message: redactImportMessage(result?.message || row.message, rows),
    }
  })
}
