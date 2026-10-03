// Tooltips keep the original quantity while the primary value uses K/M/B/T.
// Missing usage is unknown, including null and empty strings.
export function logExactTokenCount(value) {
  if (!['number', 'string'].includes(typeof value) || String(value).trim() === '') return '—'
  if (typeof value === 'string' && /^\d+$/.test(value)) return BigInt(value).toLocaleString('en-US')
  const number = Number(value)
  return Number.isFinite(number) && number >= 0 ? number.toLocaleString('en-US', { maximumFractionDigits: 20 }) : '—'
}
