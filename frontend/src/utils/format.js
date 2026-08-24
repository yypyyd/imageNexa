// Date/relative-time formatting helpers ported from admin.html.
const CN_LOCALE = 'zh-CN'
const CN_TZ_OPTS = { timeZone: 'Asia/Shanghai', hour12: false }

/** Format a unix-seconds timestamp in Asia/Shanghai. */
export function fmtTs(sec) {
  sec = Number(sec)
  if (!sec || Number.isNaN(sec)) return '—'
  try {
    return new Date(sec * 1000).toLocaleString(CN_LOCALE, {
      ...CN_TZ_OPTS, year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    })
  } catch { return '—' }
}

/** Format an ISO-8601 string in Asia/Shanghai. */
export function fmtIso(iso) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return iso
  try {
    return d.toLocaleString(CN_LOCALE, {
      ...CN_TZ_OPTS, year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
    })
  } catch { return iso }
}

// Accepts either unix-seconds (number or numeric string) or an ISO-8601 string,
// returning a Date in either case (null when unparseable). Lets the stacked
// date/time cells below work for both the unix timestamps (created_at, etc.)
// and the ISO reset_after string without callers caring which they hold.
function toDate(v) {
  if (v === null || v === undefined || v === '') return null
  if (typeof v === 'number' || /^\d+(\.\d+)?$/.test(String(v))) {
    const n = Number(v)
    return n ? new Date(n * 1000) : null
  }
  const d = new Date(v)
  return isNaN(d.getTime()) ? null : d
}

/** Date part only — "2026/06/18". Pair with fmtClock for a compact 2-line cell. */
export function fmtDate(v) {
  const d = toDate(v)
  if (!d) return '—'
  try {
    return d.toLocaleDateString(CN_LOCALE, { ...CN_TZ_OPTS, year: 'numeric', month: '2-digit', day: '2-digit' })
  } catch { return '—' }
}

/** Time part only — "00:31:09". Empty string when there's no timestamp. */
export function fmtClock(v) {
  const d = toDate(v)
  if (!d) return ''
  try {
    return d.toLocaleTimeString(CN_LOCALE, { ...CN_TZ_OPTS, hour: '2-digit', minute: '2-digit', second: '2-digit' })
  } catch { return '' }
}

// Rank a resolution tier for ascending sort. Handles both video ("720p"/"1080p"
// /"4k") and image ("1K"/"2K"/"4K"): the "k" suffix scales ×1000 so "4k"/"4K"
// rank ABOVE "1080p" (a plain parseFloat would put "4k"=4 first, which is wrong).
function resRank(r) {
  const s = String(r).trim()
  const n = parseFloat(s) || 0
  return /k$/i.test(s) ? n * 1000 : n
}

/** Sort resolution tiers ascending (720p before 1080p; 1K before 2K before 4K). */
export function sortResolutions(list) {
  return [...(list || [])].sort((a, b) => resRank(a) - resRank(b))
}

/** Hide raw Adobe JSON/provider internals for request-level moderation errors. */
export function friendlyGenerationError(value) {
  const message = String(value || '').trim()
  if (/reference_image_privacy_error|reference image contains a real person'?s face/i.test(message)) {
    return '参考图片包含真人面部，Adobe 不允许将其用于生成内容，请更换不含真人面部的图片。'
  }
  if (/image_unsafe|prompt_unsafe|adobe content rejected/i.test(message)) {
    return 'Adobe 内容安全审核未通过，请修改提示词或参考素材后重试。'
  }
  return message
}

/** Human-readable byte size — "86 MB", "5.3 MB", "512 KB". Rounds to a whole
 *  number at ≥10 units, one decimal below, so the same byte count reads
 *  identically everywhere (overview KPI, 图片管理, lightbox…). */
export function fmtSize(bytes) {
  bytes = Number(bytes)
  if (!bytes || Number.isNaN(bytes)) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0; let v = bytes
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${u[i]}`
}
