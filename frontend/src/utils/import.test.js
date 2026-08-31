import assert from 'node:assert/strict'
import test from 'node:test'
import { strToU8, zipSync } from 'fflate'
import { parseImportFileBytes, parseImportInput } from './import.js'

function jwt(email) {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'none' })}.${encode({
    'https://api.openai.com/profile': { email },
    'https://api.openai.com/auth': { chatgpt_plan_type: 'free' },
  })}.signature`
}

function grokJwt(sessionId) {
  const encode = (value) => Buffer.from(JSON.stringify(value)).toString('base64url')
  return `${encode({ alg: 'none' })}.${encode({ session_id: sessionId })}.signature`
}

test('parses a CPA codex auth file', () => {
  const token = jwt('cpa@example.com')
  const items = parseImportInput(JSON.stringify({
    type: 'codex',
    access_token: token,
    refresh_token: '',
    account_id: 'account-1',
  }))
  assert.deepEqual(items, [{ type: 'openai', value: token }])
})

test('parses Sub2API bundle and skips Agent Identity-only accounts', () => {
  const tokenA = jwt('a@example.com')
  const tokenB = jwt('b@example.com')
  const items = parseImportInput(JSON.stringify({
    exported_at: '2026-07-25T00:00:00Z',
    accounts: [
      { platform: 'openai', credentials: { auth_mode: 'chatgpt', access_token: tokenA } },
      { platform: 'openai', credentials: { auth_mode: 'agentIdentity', agent_private_key: 'private' } },
      { platform: 'openai', credentials: { auth_mode: 'chatgpt', access_token: tokenB } },
    ],
  }))
  assert.deepEqual(items, [
    { type: 'openai', value: tokenA },
    { type: 'openai', value: tokenB },
  ])
})

test('parses and deduplicates CPA JSON files from a ZIP', () => {
  const token = jwt('zip@example.com')
  const auth = strToU8(JSON.stringify({ type: 'codex', access_token: token }))
  const archive = zipSync({
    'codex-a.json': auth,
    'nested/codex-b.json': auth,
    'readme.txt': strToU8('ignored'),
  })
  assert.deepEqual(parseImportFileBytes(archive, 'accounts.zip'), [{ type: 'openai', value: token }])
})

test('rejects Agent Identity-only files with an actionable message', () => {
  const data = strToU8(JSON.stringify({
    type: 'codex',
    auth_mode: 'agentIdentity',
    agent_private_key: 'private',
  }))
  assert.throws(() => parseImportFileBytes(data, 'agent.json'), /没有识别到可导入的账号凭据/)
})

test('parses grok2api sso pool exports', () => {
  const basicA = grokJwt('basic-a')
  const basicB = grokJwt('basic-b')
  const superToken = grokJwt('super-a')
  const data = strToU8(JSON.stringify({
    ssoBasic: [
      { token: basicA, note: '', tags: ['free'] },
      { token: basicB, note: 'second', tags: [] },
      { token: basicA, note: 'duplicate', tags: [] },
      { token: 'not-a-grok-token', note: 'invalid', tags: [] },
    ],
    ssoSuper: [{ token: superToken, note: '', tags: [] }],
  }))
  assert.deepEqual(parseImportFileBytes(data, 'grok2api_token.json'), [
    { type: 'grok', value: basicA },
    { type: 'grok', value: basicB },
    { type: 'grok', value: superToken },
  ])
})

test('keeps existing credential types in mixed JSON arrays', () => {
  const token = jwt('mixed@example.com')
  const imagine = { token: jwt('imagine@example.com'), refreshToken: jwt('refresh@example.com') }
  const items = parseImportInput(JSON.stringify([
    { type: 'codex', access_token: token },
    imagine,
    { cookie: 'better-auth.session_token=leonardo-cookie' },
  ]))
  assert.equal(items[0].type, 'openai')
  assert.equal(items[1].type, 'imagine')
  assert.equal(items[2].type, 'leonardo')
})

test('classifies a BytePlus Lumina cookie before the Adobe fallback', () => {
  const cookie = 'sessionid=byteplus-session; csrfToken=byteplus-csrf; locale=en-US'
  assert.deepEqual(parseImportInput(cookie), [{ type: 'byteplus', value: cookie }])
  assert.deepEqual(parseImportInput('csrfToken=anonymous; locale=en-US; __spti=tracking'), [{
    type: 'adobe',
    value: 'csrfToken=anonymous; locale=en-US; __spti=tracking',
  }])
  assert.deepEqual(parseImportInput('sessionid=unknown-session'), [{ type: 'adobe', value: 'sessionid=unknown-session' }])
})

test('parses a BytePlus browser cookie export as one account', () => {
  const items = parseImportInput(JSON.stringify([
    { name: 'sessionid', value: 'byteplus-session' },
    { name: 'csrfToken', value: 'byteplus-csrf' },
    { name: 'locale', value: 'en-US' },
  ]))
  assert.deepEqual(items, [{
    type: 'byteplus',
    value: 'sessionid=byteplus-session; csrfToken=byteplus-csrf; locale=en-US',
  }])
})

test('parses OreateAI export without retaining its password', () => {
  const exported = {
    base_url: 'https://www.oreateai.com/',
    cookie: 'OUID=device-id; ouss=session-cookie',
    email: 'oreate@example.com',
    ouid: 'device-id',
    user_agent: 'test-agent',
    password: 'must-not-survive',
    points: 124,
  }
  const items = parseImportFileBytes(strToU8(JSON.stringify(exported)), 'oreate-account.json')
  assert.deepEqual(items, [{
    type: 'oreate',
    value: exported.cookie,
    meta: {
      email: exported.email,
      ouid: exported.ouid,
      user_agent: exported.user_agent,
      reg_ts: 0,
      vip: '0',
    },
  }])
  assert.equal(JSON.stringify(items).includes(exported.password), false)
  assert.equal(JSON.stringify(items).includes('password'), false)
})
