export const TEXT_MODELS = Object.freeze([
  'gpt-5-5-mini',
  'gpt-5-5-thinking',
  'grok-4.5',
  'grok-chat-fast',
])

export const IMAGE_MODELS = Object.freeze([
  'gpt-image-2',
  'seedream-5.0-pro',
  'seedream-5.0-lite',
  'nano-banana-2',
  'nano-banana-pro',
  'grok-imagine-image',
])

export const VIDEO_MODELS = Object.freeze([
  'veo-3.1',
  'veo-3.1-lite',
  'kling-3',
  'kling-o3',
  'runway-gen-4.5',
  'runway-gen-4-turbo',
  'seedance-2.0',
  'seedance-2.0-fast',
  'seedance-2.0-mini',
  'seedance-1.5-pro',
  'seedance-2.5',
  'grok-imagine-video',
  'luma-ray',
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
