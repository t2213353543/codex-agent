// DateTimeFormat construction is expensive in long log and daily-stat tables.
// Bound the cache because the IANA timezone comes from an editable filter.
const formatters = new Map()
const limit = 32

export function timezoneFormatter(timezone, dateOnly = false) {
  const key = `${dateOnly ? 'date' : 'time'}:${timezone}`
  let formatter = formatters.get(key)
  if (!formatter) {
    formatter = new Intl.DateTimeFormat('en-CA', {
      timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit',
      ...(!dateOnly ? { hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' } : {}),
    })
    formatters.set(key, formatter)
    if (formatters.size > limit) formatters.delete(formatters.keys().next().value)
  }
  return formatter
}
