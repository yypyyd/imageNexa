export const TEXT_MODELS = Object.freeze([
  'gpt-5-5-mini',
  'gpt-5-5-thinking',
  'grok-4.5',
  'grok-chat-fast',
])

export const IMAGE_MODELS = Object.freeze([
  'chatgpt-gpt-image-2',
  'byteplus-gpt-image-2',
  'adobe-gpt-image-2',
  'seedream-5.0-pro',
  'seedream-5.0-lite',
  'byteplus-nano-banana-2',
  'adobe-nano-banana-2',
  'byteplus-nano-banana-pro',
  'adobe-nano-banana-pro',
  'grok-imagine-image',
])

export const VIDEO_MODELS = Object.freeze([
  'kling-3',
  'kling-o3',
  'adobe-seedance-2.0',
  'oreate-seedance-2.0',
  'adobe-seedance-2.0-fast',
  'oreate-seedance-2.0-fast',
  'seedance-2.0-mini',
  'seedance-1.5-pro',
  'oreate-seedance-2.5',
  'dola-seedance-2.5',
  'grok-imagine-video',
  'firefly-video',
])

export const MODEL_KIND = Object.freeze(Object.fromEntries([
  ...TEXT_MODELS.map((id) => [id, 'text']),
  ...IMAGE_MODELS.map((id) => [id, 'image']),
  ...VIDEO_MODELS.map((id) => [id, 'video']),
]))

export const ALL_MODELS = Object.freeze(Object.keys(MODEL_KIND))

export function isCanonicalModel(id) {
  return Object.hasOwn(MODEL_KIND, id)
}
