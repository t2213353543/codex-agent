import { request } from '../api.js'
import { ingressQuery } from '../utils/logIngressState.js'

export const readIngressEvents = (filters, limit, cursor, options = {}) => request(`/ingress-events?${new URLSearchParams(ingressQuery(filters, limit, cursor))}`, { ...options, silent: true })
