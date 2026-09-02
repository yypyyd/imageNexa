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

test('distinguishes ChatGPT, Runway, and Grok JWT claims', () => {
  const chatgpt = jwt({ 'https://api.openai.com/auth': { user_id: 'u1' } })
  const runway = jwt({ id: 123, sso: true })
  const grok = jwt({ session_id: 'session-1' })
  assert.deepEqual(parseCredentialImports(`${chatgpt}\n${runway}\n${grok}`).map((item) => item.provider), [
    'chatgpt', 'runway', 'grok',
  ])
})

test('auto-detects BytePlus, OreateAI, and Adobe cookies', () => {
  const imports = parseCredentialImports([
    'csrfToken=csrf; sessionid=session',
    'OUID=user; ouss=session',
    'AdobeAuth=opaque; other=value',
  ].join('\n'))
  assert.deepEqual(imports.map((item) => item.provider), ['byteplus', 'oreate', 'adobe'])
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

test('does not retain OreateAI export passwords', () => {
  const exported = {
    cookie: 'OUID=device-id; ouss=session-cookie',
    email: 'oreate@example.com',
    password: 'must-not-survive',
  }
  const items = parseCredentialImports(JSON.stringify(exported))
  assert.equal(items[0].provider, 'oreate')
  assert.equal(JSON.stringify(items).includes(exported.password), false)
  assert.equal(JSON.stringify(items).includes('password'), false)
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
