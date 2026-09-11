import assert from 'node:assert/strict'
import test from 'node:test'
import { strToU8, zipSync } from 'fflate'
import { parseCredentialFileBytes, parseCredentialImports, parseCredentialInput } from './credential.js'

test('parses complete provider credential JSON as structured data', () => {
  const input = '  {"cookie_string":"sid=abc","cookies":[{"name":"sid","value":"abc"}]}  '
  assert.deepEqual(parseCredentialInput(input), {
    cookie_string: 'sid=abc',
    cookies: [{ name: 'sid', value: 'abc' }],
  })
})

test('keeps raw token and cookie credentials as trimmed strings', () => {
  assert.equal(parseCredentialInput('  sid=abc; csrf=xyz  '), 'sid=abc; csrf=xyz')
  assert.equal(parseCredentialInput('  "token-in-quotes"  '), '"token-in-quotes"')
})

function jwt(claims) {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'none' })}.${encode(claims)}.signature`
}

test('auto-detects ChatGPT account exports without a provider choice', () => {
  const accessToken = jwt({ 'https://api.openai.com/profile': { email: 'user@example.com' } })
  const input = JSON.stringify([{
    auth_mode: 'chatgpt',
    access_token: accessToken,
    email: 'user@example.com',
    plan_type: 'free',
  }])
  assert.deepEqual(parseCredentialImports(input), [{ provider: 'chatgpt', credential: accessToken }])
})

test('distinguishes ChatGPT and Grok JWT claims and ignores Runway', () => {
  const chatgpt = jwt({ 'https://api.openai.com/auth': { user_id: 'u1' } })
  const runway = jwt({ id: 123, sso: true })
  const grok = jwt({ session_id: 'session-1' })
  assert.deepEqual(parseCredentialImports(`${chatgpt}\n${runway}\n${grok}`).map((item) => item.provider), [
    'chatgpt', 'grok',
  ])
})

test('does not classify Sub2API Grok accounts as ChatGPT', () => {
  const sso = jwt({ session_id: 'grok-session' })
  const oauth = jwt({ iss: 'https://auth.x.ai', sub: 'user-1', client_id: 'grok-cli' })
  const input = JSON.stringify({
    type: 'sub2api-data',
    version: 1,
    accounts: [
      { platform: 'grok', type: 'oauth', credentials: { sso } },
      { platform: 'grok', type: 'oauth', credentials: { access_token: oauth, refresh_token: 'refresh' } },
    ],
  })
  assert.deepEqual(parseCredentialImports(input).map((item) => item.provider), ['grok', 'grok'])
})

test('keeps ChatGPT Sub2API accounts on ChatGPT', () => {
  const accessToken = jwt({ 'https://api.openai.com/profile': { email: 'user@example.com' } })
  const input = JSON.stringify({
    type: 'sub2api-data',
    accounts: [{
      platform: 'openai',
      type: 'oauth',
      credentials: { access_token: accessToken, auth_mode: 'chatgpt' },
    }],
  })
  assert.deepEqual(parseCredentialImports(input), [{ provider: 'chatgpt', credential: accessToken }])
})

test('treats xAI access tokens as Grok even without a platform field', () => {
  const oauth = jwt({ iss: 'https://auth.x.ai', sub: 'user-2' })
  assert.deepEqual(parseCredentialImports(JSON.stringify({ access_token: oauth })), [
    { provider: 'grok', credential: oauth },
  ])
})

test('does not treat Sub2API Grok OAuth exports as ChatGPT', () => {
  const grokSSO = jwt({ session_id: 'session-281' })
  const grokOAuth = jwt({ iss: 'https://auth.x.ai', sub: 'user-1', client_id: 'grok-cli' })
  const input = JSON.stringify({
    type: 'sub2api-data',
    version: 1,
    accounts: [
      { platform: 'grok', type: 'oauth', name: 'a@x.ai', credentials: { access_token: grokOAuth } },
      { platform: 'grok', type: 'oauth', credentials: { sso: grokSSO, access_token: grokOAuth } },
    ],
  })
  const items = parseCredentialImports(input)
  assert.deepEqual(items.map((item) => item.provider), ['grok', 'grok'])
  assert.equal(items[1].credential, grokSSO)
})

test('still imports Sub2API ChatGPT accounts as ChatGPT', () => {
  const accessToken = jwt({ 'https://api.openai.com/profile': { email: 'user@example.com' } })
  const items = parseCredentialImports(JSON.stringify({
    type: 'sub2api-data',
    accounts: [{
      platform: 'openai',
      credentials: { access_token: accessToken, auth_mode: 'chatgpt' },
    }],
  }))
  assert.deepEqual(items, [{ provider: 'chatgpt', credential: accessToken }])
})

test('classifies xAI issuer JWTs as Grok even without session_id', () => {
  const token = jwt({ iss: 'https://auth.x.ai', sub: 'acct' })
  assert.deepEqual(parseCredentialImports(token).map((item) => item.provider), ['grok'])
})

test('auto-detects Dola cookies by sessionid plus s_v_web_id', () => {
  const cookie = 'sessionid=abc; sessionid_ss=abc; s_v_web_id=verify%2Fx; msToken=ENIAMtoken; ttwid=1%7Cx'
  const imports = parseCredentialImports(cookie)
  assert.equal(imports.length, 1)
  assert.equal(imports[0].provider, 'dola')
  assert.equal(imports[0].credential, cookie)
})

test('auto-detects BytePlus and Adobe cookies and ignores OreateAI', () => {
  const imports = parseCredentialImports([
    'csrfToken=csrf; sessionid=session',
    'OUID=user; ouss=session',
    'AdobeAuth=opaque; other=value',
  ].join('\n'))
  assert.deepEqual(imports.map((item) => item.provider), ['byteplus', 'adobe'])
})

test('recognizes Passport login-only Dola exports without a fingerprint cookie', () => {
  const cookie = 'passport_csrf_token=test; sid_guard=guard; sid_tt=session; sessionid=session; store-idc=mya; store-country-code=hk'
  for (const input of [cookie, JSON.stringify({ cookie }), JSON.stringify(cookie.split('; ').map(pair => {
    const [name, value] = pair.split('='); return { name, value }
  }))]) {
    assert.deepEqual(parseCredentialImports(input), [{ provider: 'dola', credential: cookie }])
  }
  assert.notEqual(parseCredentialImports('sessionid=session; sid_tt=other; sid_guard=guard; passport_csrf_token=test; store-idc=mya')[0].provider, 'dola')
  assert.notEqual(parseCredentialImports('sessionid=session; passport_csrf_token=test')[0].provider, 'dola')
})

test('treats a browser cookie array as one account', () => {
  const input = JSON.stringify([
    { name: 'csrfToken', value: 'csrf' },
    { name: 'sessionid', value: 'session' },
  ])
  const imports = parseCredentialImports(input)
  assert.equal(imports.length, 1)
  assert.equal(imports[0].provider, 'byteplus')
})

test('preserves Lumina login material for private server-side renewal', () => {
  const exported = {
    platform: 'lumina',
    email: 'lumina@example.com',
    cookie_string: 'csrfToken=csrf; digest=jwt; AccountID=account-1',
    session_expires: 1788500000,
    session_expires_at: '2026-09-04T05:33:20Z',
    password: 'must-not-survive',
  }
  const items = parseCredentialImports(JSON.stringify(exported))
  assert.deepEqual(items, [{
    provider: 'byteplus',
    credential: {
      cookie_string: exported.cookie_string,
      email: exported.email,
      password: exported.password,
      session_expires_at: exported.session_expires_at,
      session_expires: exported.session_expires,
    },
  }])
  assert.equal(JSON.stringify(items).includes(exported.password), true)
})

test('parses and deduplicates account JSON files from a ZIP', () => {
  const token = jwt({ 'https://api.openai.com/profile': { email: 'zip@example.com' } })
  const auth = strToU8(JSON.stringify({ type: 'codex', access_token: token }))
  const archive = zipSync({
    'codex-a.json': auth,
    'nested/codex-b.json': auth,
    'readme.txt': strToU8('ignored'),
  })
  assert.deepEqual(parseCredentialFileBytes(archive, 'accounts.zip'), [{ provider: 'chatgpt', credential: token }])
})

test('rejects OreateAI exports from account import', () => {
  const exported = {
    cookie: 'OUID=device-id; ouss=session-cookie',
    email: 'oreate@example.com',
    password: 'must-not-survive',
  }
  const items = parseCredentialImports(JSON.stringify(exported))
  assert.deepEqual(items, [])
  assert.equal(JSON.stringify(items).includes(exported.password), false)
})

test('does not misclassify an isolated csrf cookie as BytePlus', () => {
  const imports = parseCredentialImports('csrfToken=anonymous; locale=en-US; __spti=tracking')
  assert.equal(imports[0].provider, 'adobe')
})

test('accepts exported cookie objects that store the cookie in value', () => {
  const imports = parseCredentialImports(JSON.stringify({ value: 'AdobeAuth=opaque' }))
  assert.deepEqual(imports, [{ provider: 'adobe', credential: 'AdobeAuth=opaque' }])
})

test('preserves an Adobe ARP session header with its cookie', () => {
  const imports = parseCredentialImports(JSON.stringify({
    cookie: 'AdobeAuth=opaque',
    headers: { 'X-ARP-Session-ID': 'arp-real-token' },
  }))
  assert.deepEqual(imports, [{
    provider: 'adobe',
    credential: { cookie: 'AdobeAuth=opaque', arp_session_token: 'arp-real-token' },
  }])
})

test('accepts the Adobe arpSessionToken export alias', () => {
  const imports = parseCredentialImports(JSON.stringify({
    cookie_string: 'AdobeAuth=opaque',
    arpSessionToken: 'arp-alias-token',
  }))
  assert.equal(imports[0].credential.arp_session_token, 'arp-alias-token')
})
