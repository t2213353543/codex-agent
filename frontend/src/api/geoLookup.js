import { request } from '../api.js'
import { geoLookupBody } from '../utils/geoLookupState.js'

// Call only from the explicit GeoJS query/refresh action. Importing this module
// and rendering a log row never sends an IP to any service.
export function lookupIPGeo(ips, { force = false, signal } = {}) {
  return request('/logs/ip-geo', { method: 'POST', body: geoLookupBody(ips, force), signal, silent: true })
}
