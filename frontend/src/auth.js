import { reactive } from 'vue'

const BASE = import.meta.env?.VITE_API_BASE || ''

export const auth = reactive({
  admin: null,
  initialized: null,
  bootstrapTokenRequired: false,
  ready: false,
  csrfToken: '',
})

let csrfPromise = null

function authURL(path) {
  return `${BASE}/admin/api/auth/${path}`
}

async function parseJSON(response) {
  try { return await response.json() } catch { return null }
}

export function clearSession() {
  auth.admin = null
  auth.csrfToken = ''
}

async function loadAuthStatus() {
  try {
    const response = await fetch(authURL('status'), {
      credentials: 'include',
      headers: { Accept: 'application/json' },
    })
    const data = await parseJSON(response)
    if (response.ok) {
      if (typeof data?.initialized === 'boolean') auth.initialized = data.initialized
      else if (typeof data?.needs_initialization === 'boolean') auth.initialized = !data.needs_initialization
      else if (typeof data?.initialization_open === 'boolean') auth.initialized = !data.initialization_open
      auth.bootstrapTokenRequired = typeof data?.bootstrap_token_required === 'boolean'
        ? data.bootstrap_token_required
        : auth.initialized === false
    }
  } catch {
    // Keep the state unknown so the bootstrap screen can offer a retry.
  }
  return auth.initialized
}

async function refreshMe() {
  try {
    const response = await fetch(authURL('me'), {
      credentials: 'include',
      headers: { Accept: 'application/json' },
    })
    const data = await parseJSON(response)
    if (response.ok) {
      auth.admin = data?.admin || data?.user || data
      if (data?.csrf_token) auth.csrfToken = data.csrf_token
    }
    else if (response.status === 401) clearSession()
  } catch {
    // A temporary network failure must not manufacture a logout.
  }
  return auth.admin
}

export async function bootstrapAuth() {
  if (auth.ready) return auth
  await loadAuthStatus()
  if (auth.initialized) await refreshMe()
  auth.ready = true
  return auth
}

export async function getCSRF(force = false) {
  if (auth.csrfToken && !force) return auth.csrfToken
  if (csrfPromise && !force) return csrfPromise

  csrfPromise = (async () => {
    const response = await fetch(authURL('csrf'), {
      credentials: 'include',
      headers: { Accept: 'application/json' },
    })
    const data = await parseJSON(response)
    if (!response.ok) throw new Error(data?.error?.message || data?.detail || '无法获取 CSRF Token')
    const token = data?.csrf_token || data?.token || response.headers.get('X-CSRF-Token') || ''
    if (!token) throw new Error('服务端未返回 CSRF Token')
    auth.csrfToken = token
    return token
  })()

  try { return await csrfPromise } finally { csrfPromise = null }
}

async function authWrite(path, payload, additionalHeaders = {}) {
  // Login and one-time initialization have no session yet. The backend protects
  // those two writes with an exact Origin check; session CSRF starts only after
  // a successful response. All other auth writes require the per-session token.
  const needsCSRF = path !== 'login' && path !== 'initialize'
  const token = needsCSRF ? await getCSRF() : ''
  const headers = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    ...additionalHeaders,
  }
  if (token) headers['X-CSRF-Token'] = token
  const response = await fetch(authURL(path), {
    method: 'POST',
    credentials: 'include',
    headers,
    body: JSON.stringify(payload),
  })
  const data = await parseJSON(response)
  if (!response.ok) {
    const error = new Error(data?.error?.message || data?.detail || data?.message || '请求失败')
    error.status = response.status
    throw error
  }
  if (path === 'login' || path === 'initialize') {
    auth.initialized = true
    auth.admin = data?.admin || data?.user || null
    auth.csrfToken = data?.csrf_token || ''
    if (!auth.admin) await refreshMe()
  }
  return data
}

export function login(identifier, password) {
  return authWrite('login', { identifier, username: identifier, password })
}

export function initializeAdmin({ username, email, password, bootstrapToken }) {
  return authWrite(
    'initialize',
    { username, email, password },
    { 'X-Admin-Bootstrap-Token': bootstrapToken },
  )
}

export async function logout() {
  try { await authWrite('logout', {}) } finally { clearSession() }
}
