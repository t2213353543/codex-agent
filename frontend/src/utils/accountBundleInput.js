// Ignore retired optional metadata in memory; keep the user's original backup
// and its credential, resource and lease data untouched.
export function accountBundleInput(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return value
  const copy = { ...value }
  if (value.management && typeof value.management === 'object' && !Array.isArray(value.management)) {
    const { costs: ignored, ...management } = value.management
    if (Object.keys(management).every(key => key === 'version')) delete copy.management
    else copy.management = management
  }
  return copy
}
