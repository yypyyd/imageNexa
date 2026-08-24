// Client-side auth state. The session token lives in localStorage and is sent
// as `Authorization: Bearer <token>` on every admin API call (see api.js).
// The server slides the 24h session whenever it's used with <22h left, so an
// active admin never gets logged out; we also re-validate via /me on a timer.
import { reactive } from 'vue'

const TOKEN_KEY = 'gw_token'
const BASE = import.meta.env.VITE_API_BASE || ''

export const auth = reactive({
  token: localStorage.getItem(TOKEN_KEY) || '',
  user: null,        // { id, email, name, role, status, credits, invite_code, ... }
  ready: false,      // true once an initial /me check has resolved
  loginOpen: false,  // is the login modal showing?
  loginIntent: '',   // where to go after a successful login
  startMode: 'login',// which tab the modal opens on: 'login' | 'register'
  pendingInvite: '', // invite code from a /?ref=CODE link, sent with register
})

export function isAuthed() { return !!auth.token && !!auth.user }
export function isAdmin() { return isAuthed() && auth.user.role === 'admin' }

/** Open the login modal, remembering where the user wanted to go. */
export function openLogin(intent = '') { auth.loginIntent = intent || ''; auth.startMode = 'login'; auth.loginOpen = true }
/** Open straight to the register tab, carrying an optional invite code.
 *  Default landing is the home page — an invitee clicking a /?ref=CODE
 *  link should NOT be punted into the画图 flow before they've explored. */
export function openRegister(inviteCode = '', intent = '/') {
  auth.pendingInvite = inviteCode || ''
  auth.loginIntent = intent || ''
  auth.startMode = 'register'
  auth.loginOpen = true
}
export function closeLogin() { auth.loginOpen = false }

export function setSession(token, user) {
  auth.token = token || ''
  auth.user = user || null
  if (token) localStorage.setItem(TOKEN_KEY, token)
  else localStorage.removeItem(TOKEN_KEY)
}

export function clearSession() {
  auth.token = ''
  auth.user = null
  localStorage.removeItem(TOKEN_KEY)
}

/** Validate the stored token against /me. Refreshes auth.user; clears on 401.
 *  Returns the user (or null). Hitting /me also slides the server session. */
export async function refreshMe() {
  if (!auth.token) { auth.user = null; auth.ready = true; return null }
  try {
    const r = await fetch(`${BASE}/admin/api/auth/me`, {
      headers: { Authorization: `Bearer ${auth.token}` },
    })
    if (r.ok) {
      const d = await r.json()
      auth.user = d.user
    } else {
      clearSession()
    }
  } catch {
    // network error — keep the token, don't force a logout
  }
  auth.ready = true
  return auth.user
}

export async function logout() {
  if (auth.token) {
    try {
      await fetch(`${BASE}/admin/api/auth/logout`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${auth.token}` },
      })
    } catch { /* ignore */ }
  }
  clearSession()
}

// Keep the session warm: re-validate every 10 minutes while a tab is open.
setInterval(() => { if (auth.token) refreshMe() }, 10 * 60 * 1000)
