import assert from 'node:assert/strict'
import test from 'node:test'
import { auth, clearSession, initializeAdmin, login } from './auth.js'

function jsonResponse(payload, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

test('login does not request an authenticated CSRF token first', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
    clearSession()
    auth.initialized = null
  })

  const calls = []
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), options })
    return jsonResponse({
      admin: { id: 'admin', username: 'admin' },
      csrf_token: 'session-csrf',
    })
  }

  await login('admin', 'a-valid-password')

  assert.equal(calls.length, 1)
  assert.match(calls[0].url, /\/admin\/api\/auth\/login$/)
  assert.equal(calls[0].options.headers['X-CSRF-Token'], undefined)
  assert.equal(auth.csrfToken, 'session-csrf')
})

test('one-time initialization sends the bootstrap secret only in its fixed header', async (t) => {
  const originalFetch = globalThis.fetch
  t.after(() => {
    globalThis.fetch = originalFetch
    clearSession()
    auth.initialized = null
  })

  const calls = []
  globalThis.fetch = async (url, options) => {
    calls.push({ url: String(url), options })
    return jsonResponse({
      admin: { id: 'admin', username: 'root' },
      csrf_token: 'initialized-csrf',
    })
  }

  await initializeAdmin({
    username: 'root',
    email: '',
    password: 'a-valid-password',
    bootstrapToken: 'one-time-bootstrap-secret',
  })

  assert.equal(calls.length, 1)
  assert.match(calls[0].url, /\/admin\/api\/auth\/initialize$/)
  assert.equal(calls[0].options.headers['X-CSRF-Token'], undefined)
  assert.equal(calls[0].options.headers['X-Admin-Bootstrap-Token'], 'one-time-bootstrap-secret')
  assert.equal(calls[0].options.body.includes('one-time-bootstrap-secret'), false)
})
