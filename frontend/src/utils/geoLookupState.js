// Local classification only. The backend revalidates every literal and remains
// authoritative; none of these helpers perform a network request or DNS lookup.
export function normalizeGeoIP(value) {
  if (typeof value !== 'string' || !value || value.length > 64 || value !== value.trim()) return ''
  if (!value.includes(':')) {
    if (!/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(value)) return ''
    const bytes = value.split('.').map(Number)
    return bytes.every(byte => byte <= 255) ? bytes.join('.') : ''
  }
  if (!/^[0-9a-fA-F:.]+$/.test(value)) return ''
  try {
    const address = new URL(`http://[${value}]/`).hostname.slice(1, -1)
    const words = expandIPv6(address)
    if (words.slice(0, 5).every(word => word === 0) && words[5] === 65535) {
      return [words[6] >> 8, words[6] & 255, words[7] >> 8, words[7] & 255].join('.')
    }
    return address
  } catch { return '' }
}

function expandIPv6(value) {
  const sides = value.split('::')
  const left = sides[0] ? sides[0].split(':').map(word => parseInt(word, 16)) : []
  const right = sides[1] ? sides[1].split(':').map(word => parseInt(word, 16)) : []
  return sides.length === 1 ? left : [...left, ...Array(8 - left.length - right.length).fill(0), ...right]
}

export function geoIPState(value) {
  const ip = normalizeGeoIP(value)
  if (!ip) return { ip: '', status: 'invalid' }
  if (ip.includes(':')) {
    const [a, b] = expandIPv6(ip)
    const publicIP = (a & 0xe000) === 0x2000 && !(a === 0x2001 && (b & 0xfe00) === 0) && !(a === 0x2001 && b === 0x0db8) && a !== 0x2002 && !(a === 0x3fff && (b & 0xf000) === 0)
    return { ip, status: publicIP ? 'idle' : 'private' }
  }
  const [a, b, c] = ip.split('.').map(Number)
  const blocked = a === 0 || a === 10 || a === 127 || a >= 224 ||
    (a === 100 && b >= 64 && b <= 127) || (a === 169 && b === 254) ||
    (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168) ||
    (a === 192 && b === 0 && (c === 0 || c === 2)) || (a === 192 && b === 88 && c === 99) ||
    (a === 198 && (b === 18 || b === 19)) || (a === 198 && b === 51 && c === 100) ||
    (a === 203 && b === 0 && c === 113)
  return { ip, status: blocked ? 'private' : 'idle' }
}

export function geoLookupBody(values, force = false) {
  if (!Array.isArray(values) || values.length < 1 || values.length > 50) throw new Error('每次请选择 1–50 个 IP')
  const ips = values.map(normalizeGeoIP)
  if (ips.some(ip => !ip)) throw new Error('仅支持不含端口或区域标识的完整 IPv4 / IPv6 地址')
  return { ips: [...new Set(ips)], force: force === true }
}

export function readGeoEntry(payload, requestedIP) {
  const canonical = normalizeGeoIP(requestedIP)
  if (!canonical || !Array.isArray(payload?.items)) throw new Error('服务器返回了无效归属地结果')
  const matches = payload.items.filter(item => normalizeGeoIP(item?.ip) === canonical)
  if (matches.length !== 1 || !['success', 'private', 'error'].includes(matches[0].status)) throw new Error('归属地结果与查询 IP 不匹配')
  const entry = matches[0]
  if (entry.status === 'success' && (entry.source !== 'GeoJS' || !/^[A-Z]{2}$/.test(entry.country_code || '') || !Number.isSafeInteger(entry.fetched_at) || entry.fetched_at <= 0 || !Number.isSafeInteger(entry.expires_at) || entry.expires_at <= entry.fetched_at)) throw new Error('归属地结果缺少来源或查询时间')
  return { ...entry, ip: canonical }
}

export function geoLocationLabel(entry) {
  return [...new Set([entry?.country || entry?.country_code, entry?.region, entry?.city].filter(value => typeof value === 'string' && value.trim()))].join(' · ')
}

export function planGeoBatch(values) {
  if (!Array.isArray(values) || values.length > 500) throw new Error('本页批量查询最多支持 500 条记录，请减小每页数量')
  const plan = { ips: [], private_count: 0, invalid_count: 0, missing_count: 0, duplicate_count: 0 }
  const seen = new Set()
  for (const raw of values) {
    if (!raw) { plan.missing_count++; continue }
    const item = geoIPState(raw)
    if (item.status === 'invalid') { plan.invalid_count++; continue }
    if (seen.has(item.ip)) { plan.duplicate_count++; continue }
    seen.add(item.ip)
    if (item.status === 'private') plan.private_count++
    else plan.ips.push(item.ip)
  }
  return plan
}

// Freeze the explicit selection before the first await. No automatic retries;
// failures remain per-IP errors and cancellation retains only completed batches.
export async function runGeoBatch(values, request, { signal, onBatch, force = false } = {}) {
  const targets = [...planGeoBatch(values).ips]
  const entries = []
  for (let start = 0; start < targets.length; start += 50) {
    if (signal?.aborted) throw new DOMException('Request aborted', 'AbortError')
    const chunk = targets.slice(start, start + 50)
    let batch
    try {
      const payload = await request(chunk, { signal, force })
      if (signal?.aborted) throw new DOMException('Request aborted', 'AbortError')
      batch = chunk.map(ip => {
        try { return readGeoEntry(payload, ip) }
        catch { return { ip, status: 'error', error_code: 'invalid_response', error: '归属地结果与查询 IP 不匹配或缺少来源' } }
      })
    } catch (failure) {
      if (signal?.aborted || failure?.name === 'AbortError' || failure?.status === 401) throw failure
      batch = chunk.map(ip => ({ ip, status: 'error', error_code: 'upstream_unavailable', error: failure?.message || 'GeoJS 查询失败' }))
    }
    entries.push(...batch)
    onBatch?.(batch)
  }
  return entries
}
