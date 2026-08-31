/**
 * Preserve simple token/cookie input as text, while allowing complete browser
 * exports and provider credential bundles to reach the API as structured JSON.
 */
export function parseCredentialInput(value) {
  const trimmed = String(value ?? '').trim()
  if (!trimmed) return ''

  try {
    const parsed = JSON.parse(trimmed)
    return parsed !== null && typeof parsed === 'object' ? parsed : trimmed
  } catch {
    return trimmed
  }
}
