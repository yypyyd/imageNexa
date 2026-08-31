import { auth, clearSession, getCSRF } from './auth'

const BASE = import.meta.env.VITE_API_BASE || ''
const WRITE_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

function messageOf(data, fallback = '请求失败') {
  return data?.error?.message || data?.detail || data?.message || fallback
}

async function parseBody(response) {
  const contentType = response.headers.get('content-type') || ''
  if (contentType.includes('application/json')) {
    try { return await response.json() } catch { return null }
  }
  try {
    const text = await response.text()
    return text ? { message: text } : null
  } catch { return null }
}

export async function api(path, opts = {}) {
  const method = String(opts.method || 'GET').toUpperCase()
  const headers = { Accept: 'application/json', ...(opts.headers || {}) }
  if (WRITE_METHODS.has(method)) headers['X-CSRF-Token'] = await getCSRF()

  const response = await fetch(`${BASE}/admin/api${path}`, {
    ...opts,
    method,
    headers,
    credentials: 'include',
  })
  const data = await parseBody(response)

  if (response.status === 401) clearSession()
  if (response.status === 403 && (data?.code === 'csrf_invalid' || /csrf/i.test(messageOf(data, '')))) {
    auth.csrfToken = ''
  }

  return { ok: response.ok, status: response.status, data, error: response.ok ? '' : messageOf(data) }
}

export function jsonBody(method, payload) {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  }
}

export function listOf(data) {
  if (Array.isArray(data)) return data
  for (const key of ['data', 'items', 'results', 'models', 'accounts', 'credentials', 'logs', 'artifacts']) {
    if (Array.isArray(data?.[key])) return data[key]
  }
  return []
}

export function apiURL(path) {
  return `${BASE}${path}`
}
