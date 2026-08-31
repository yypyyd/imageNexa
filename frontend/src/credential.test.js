import assert from 'node:assert/strict'
import test from 'node:test'
import { parseCredentialInput } from './credential.js'

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
