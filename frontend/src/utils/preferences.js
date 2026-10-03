export function readPreference(key, fallback, validate = () => true) {
  try {
    const stored = JSON.parse(localStorage.getItem(key))
    return stored?.version === 1 && validate(stored.value) ? stored.value : fallback
  } catch { return fallback }
}

export function writePreference(key, value) {
  try { localStorage.setItem(key, JSON.stringify({ version: 1, value })) } catch { /* Browser storage may be disabled. In-memory state remains usable. */ }
}

export function removePreference(key) {
  try { localStorage.removeItem(key) } catch { /* Storage is optional. */ }
}
