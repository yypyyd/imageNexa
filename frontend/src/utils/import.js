import { strFromU8, unzipSync } from 'fflate'

// Smart parsing of pasted credentials and exported account files.

const MAX_IMPORT_FILE_BYTES = 20 * 1024 * 1024
const MAX_ZIP_ENTRY_BYTES = 2 * 1024 * 1024
const MAX_ZIP_JSON_FILES = 1000

function looksLikeJwt(s) {
  s = (s || '').replace(/^Bearer\s+/i, '').trim()
  const parts = s.split('.')
  if (parts.length !== 3) return false
  return parts.every((p) => /^[A-Za-z0-9_-]+$/.test(p) && p.length > 4)
}

function decodeJwtPayload(s) {
  try {
    let p = (s || '').replace(/^Bearer\s+/i, '').trim().split('.')[1]
    if (!p) return null
    p = p.replace(/-/g, '+').replace(/_/g, '/')
    p += '='.repeat((4 - (p.length % 4)) % 4)
    return JSON.parse(atob(p))
  } catch (_) { return null }
}

// Runway JWTs carry a top-level numeric `id` plus an `sso` claim and, crucially,
// no OpenAI (https://api.openai.com/*) claims — that's what distinguishes them
// from a ChatGPT JWT, which is otherwise also an opaque three-part token.
function looksLikeRunwayJwt(s) {
  const claims = decodeJwtPayload(s)
  if (!claims || typeof claims !== 'object') return false
  if (Object.keys(claims).some((k) => k.startsWith('https://api.openai.com/'))) return false
  return 'sso' in claims && claims.id != null
}

// Grok website "sso" JWTs carry ONLY a session_id claim (no openai claims, no
// runway id/sso) — that's what tells them apart from a ChatGPT/Runway JWT.
function looksLikeGrokJwt(s) {
  const claims = decodeJwtPayload(s)
  if (!claims || typeof claims !== 'object') return false
  if (Object.keys(claims).some((k) => k.startsWith('https://api.openai.com/'))) return false
  if ('sso' in claims || claims.id != null) return false
  return 'session_id' in claims
}

// Leonardo cookies carry the better-auth session cookie — that's what tells them
// apart from an Adobe cookie (both are otherwise opaque cookie strings).
function looksLikeLeonardoCookie(s) {
  return /better-auth\.session_token/.test(s || '') || /better-auth\.session_data/.test(s || '')
}

// Krea cookies carry the Supabase auth cookie.
function looksLikeKreaCookie(s) {
  return /sb-superb-auth-token/.test(s || '')
}

function looksLikeOreateCookie(s) {
  return /(?:^|;\s*)OUID=[^;]+/.test(s || '') && /(?:^|;\s*)ouss=[^;]+/.test(s || '')
}

// An Imagine.art credential is a JSON object { token, refreshToken } (both JWTs).
function isImagineObj(o) {
  return !!o && typeof o === 'object' &&
    typeof o.token === 'string' && looksLikeJwt(o.token) &&
    typeof o.refreshToken === 'string' && looksLikeJwt(o.refreshToken)
}

// String form (a pasted JSON object on a line).
function looksLikeImagineToken(s) {
  try { return isImagineObj(JSON.parse(s)) } catch (_) { return false }
}

// Classify an opaque credential string by its distinctive shape. Imagine is
// JSON-shaped, so it must be checked before the cookie heuristics.
function cookieType(v) {
  if (looksLikeImagineToken(v)) return 'imagine'
  if (looksLikeOreateCookie(v)) return 'oreate'
  if (looksLikeKreaCookie(v)) return 'krea'
  if (looksLikeLeonardoCookie(v)) return 'leonardo'
  return 'adobe'
}

function cookieFromAny(item) {
  if (typeof item === 'string') return item.trim()
  if (item && typeof item === 'object') {
    if (typeof item.cookie === 'string') return item.cookie.trim()
    if (typeof item.value === 'string' && !('name' in item)) return item.value.trim()
    if (Array.isArray(item.cookies)) {
      return item.cookies.filter((c) => c && c.name).map((c) => `${c.name}=${c.value}`).join('; ')
    }
  }
  return ''
}

function classifyString(value) {
  const stripped = (value || '').replace(/^Bearer\s+/i, '').replace(/^sso=/, '').trim()
  if (looksLikeJwt(stripped)) {
    return looksLikeRunwayJwt(stripped)
      ? { type: 'runway', value: stripped }
      : looksLikeGrokJwt(stripped)
        ? { type: 'grok', value: stripped }
        : { type: 'openai', value: stripped }
  }
  return stripped ? { type: cookieType(stripped), value: stripped } : null
}

function openAIItem(value) {
  const token = (value || '').replace(/^Bearer\s+/i, '').trim()
  return looksLikeJwt(token) ? [{ type: 'openai', value: token }] : []
}

function oreateItem(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const cookie = cookieFromAny(value)
  const baseURL = String(value.base_url || value.baseUrl || '')
  if (!looksLikeOreateCookie(cookie) || (baseURL && !/oreateai\.com/i.test(baseURL))) return null
  return {
    type: 'oreate',
    value: cookie,
    meta: {
      email: String(value.email || '').trim(),
      ouid: String(value.ouid || '').trim(),
      user_agent: String(value.user_agent || value.userAgent || '').trim(),
      reg_ts: Number(value.reg_ts || value.createTime || 0) || 0,
      vip: String(value.vip || '0').trim(),
    },
  }
}

function grokItem(value) {
  const token = (value || '').replace(/^Bearer\s+/i, '').replace(/^sso=/, '').trim()
  return looksLikeGrokJwt(token) ? [{ type: 'grok', value: token }] : []
}

// grok2api export: { ssoBasic: [{ token, note, tags }], ssoSuper: [...] }.
// Pool names can grow as grok2api adds account tiers, so accept any sso*
// array while still validating every token by its Grok-specific JWT claims.
function structuredGrokItems(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null
  const poolKeys = Object.keys(value).filter((key) => /^sso[\w-]*$/i.test(key) && Array.isArray(value[key]))
  if (!poolKeys.length) return null

  const out = []
  for (const key of poolKeys) {
    for (const entry of value[key]) {
      if (typeof entry === 'string') {
        out.push(...grokItem(entry))
      } else if (entry && typeof entry === 'object') {
        out.push(...grokItem(entry.token || entry.sso || entry.value))
      }
    }
  }
  return out
}

// Returns null when the object is not a CPA/Sub2API shape; an empty array means
// the shape was recognized but contains no usable access_token (for example an
// Agent Identity-only credential).
function structuredOpenAIItems(value) {
  if (!value || typeof value !== 'object') return null
  if (Array.isArray(value)) return null

  // Sub2API bundle: { exported_at, accounts: [{ platform, credentials }] }.
  if (Array.isArray(value.accounts)) {
    const out = []
    for (const account of value.accounts) {
      const parsed = structuredOpenAIItems(account)
      if (parsed !== null) out.push(...parsed)
    }
    return out
  }

  // A single Sub2API account entry.
  if (value.credentials && typeof value.credentials === 'object') {
    const platform = String(value.platform || '').toLowerCase()
    const mode = String(value.credentials.auth_mode || '').toLowerCase()
    if (platform === 'openai' || mode === 'chatgpt' || mode === 'agentidentity' || 'access_token' in value.credentials) {
      return openAIItem(value.credentials.access_token)
    }
  }

  // CLIProxyAPI/Codex auth-dir JSON, plus a direct access-token object.
  const type = String(value.type || '').toLowerCase()
  const mode = String(value.auth_mode || '').toLowerCase()
  if (type === 'codex' || mode === 'chatgpt' || mode === 'agentidentity' || 'access_token' in value) {
    return openAIItem(value.access_token)
  }
  return null
}

function parseJSONValue(j) {
  // A browser cookie export is one account, not one account per cookie row.
  if (Array.isArray(j) && j.length > 0 &&
      j.every((it) => it && typeof it === 'object' && 'name' in it && 'value' in it)) {
    const joined = j.filter((c) => c && c.name).map((c) => `${c.name}=${c.value}`).join('; ')
    return joined ? [{ type: cookieType(joined), value: joined }] : []
  }

  if (Array.isArray(j)) {
    const out = []
    for (const it of j) {
      const oreate = oreateItem(it)
      if (oreate) {
        out.push(oreate)
        continue
      }
      const structured = structuredOpenAIItems(it)
      if (structured !== null) {
        out.push(...structured)
        continue
      }
      if (isImagineObj(it)) {
        out.push({ type: 'imagine', value: JSON.stringify(it) })
        continue
      }
      if (typeof it === 'string') {
        const parsed = classifyString(it)
        if (parsed) out.push(parsed)
        continue
      }
      const v = cookieFromAny(it)
      if (v) out.push({ type: cookieType(v), value: v })
    }
    return out
  }

  const structuredGrok = structuredGrokItems(j)
  if (structuredGrok !== null) return structuredGrok

  const structured = structuredOpenAIItems(j)
  if (structured !== null) return structured

  if (j && typeof j === 'object') {
    const oreate = oreateItem(j)
    if (oreate) return [oreate]
    if (isImagineObj(j)) return [{ type: 'imagine', value: JSON.stringify(j) }]
    const v = cookieFromAny(j)
    return v ? [{ type: cookieType(v), value: v }] : []
  }
  if (typeof j === 'string') {
    const item = classifyString(j)
    return item ? [item] : []
  }
  return []
}

export function uniqueImportItems(items) {
  const seen = new Set()
  return (items || []).filter((item) => {
    if (!item?.type || !item?.value) return false
    const key = `${item.type}\u0000${item.value}`
    if (seen.has(key)) return false
    seen.add(key)
    return true
  })
}

/** Returns normalized provider credentials as { type, value } items. */
export function parseImportInput(text) {
  text = (text || '').trim()
  if (!text) return []
  // Try JSON first.
  try {
    const j = JSON.parse(text)
    return uniqueImportItems(parseJSONValue(j))
  } catch (_) { /* not JSON */ }
  // Not JSON → split per line, identify each. A JWT is either a Runway token
  // (top-level id+sso, no openai claims) or a ChatGPT token; anything else is
  // treated as an Adobe cookie string.
  const lines = text.split(/\r?\n/).map((s) => s.trim()).filter(Boolean)
  return uniqueImportItems(lines.map(classifyString).filter(Boolean))
}

function parseJSONDocument(text, label) {
  let parsed
  try {
    parsed = JSON.parse(text)
  } catch (_) {
    throw new Error(`${label} 不是有效的 JSON`)
  }
  return parseJSONValue(parsed)
}

export function parseImportFileBytes(bytes, filename = 'accounts.json') {
  const data = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
  if (data.byteLength > MAX_IMPORT_FILE_BYTES) throw new Error('导入文件不能超过 20 MB')

  const isZip = /\.zip$/i.test(filename) || (data[0] === 0x50 && data[1] === 0x4b)
  let items = []
  if (isZip) {
    let count = 0
    let total = 0
    let limitError = ''
    const files = unzipSync(data, {
      filter(file) {
        if (file.name.endsWith('/') || !/\.json$/i.test(file.name)) return false
        count++
        total += Number(file.originalSize || 0)
        if (count > MAX_ZIP_JSON_FILES) limitError = 'ZIP 内 JSON 文件不能超过 1000 个'
        if (Number(file.originalSize || 0) > MAX_ZIP_ENTRY_BYTES) limitError = `${file.name} 解压后超过 2 MB`
        if (total > MAX_IMPORT_FILE_BYTES) limitError = 'ZIP 解压后的 JSON 总大小不能超过 20 MB'
        return !limitError
      },
    })
    if (limitError) throw new Error(limitError)
    for (const [name, content] of Object.entries(files)) {
      items.push(...parseJSONDocument(strFromU8(content), name))
    }
  } else {
    items = parseJSONDocument(strFromU8(data), filename)
  }

  items = uniqueImportItems(items)
  if (!items.length) {
    throw new Error('文件中没有识别到可导入的账号凭据（仅 Agent Identity 的凭据不能用于生图）')
  }
  return items
}

export async function parseImportFile(file) {
  if (!file) return []
  return parseImportFileBytes(new Uint8Array(await file.arrayBuffer()), file.name)
}
