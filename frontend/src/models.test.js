import assert from 'node:assert/strict'
import test from 'node:test'
import { ALL_MODELS, IMAGE_MODELS, MODEL_KIND, TEXT_MODELS, VIDEO_MODELS, isCanonicalModel } from './models.js'

test('canonical model catalog is closed and unique', () => {
  assert.equal(TEXT_MODELS.length, 4)
  assert.equal(IMAGE_MODELS.length, 10)
  assert.equal(VIDEO_MODELS.length, 12)
  assert.equal(new Set(ALL_MODELS).size, 26)
  assert.equal(Object.keys(MODEL_KIND).length, 26)
})

test('multi-provider products are split into per-channel public ids', () => {
  assert.deepEqual(IMAGE_MODELS, [
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
  assert.equal(isCanonicalModel('chatgpt-gpt-image-2'), true)
  assert.equal(isCanonicalModel('dola-seedance-2.5'), true)
  assert.equal(isCanonicalModel('gpt-image-2'), false)
  assert.equal(isCanonicalModel('nano-banana-2'), false)
  assert.equal(isCanonicalModel('nano-banana-pro'), false)
  assert.equal(isCanonicalModel('runway-nano-banana-2'), false)
  assert.equal(isCanonicalModel('runway-nano-banana-pro'), false)
  assert.equal(isCanonicalModel('seedance-2.0'), false)
  assert.equal(isCanonicalModel('seedance-2.5'), false)
  assert.equal(isCanonicalModel('lumina-image'), false)
  assert.equal(isCanonicalModel('firefly-image-5'), false)
  assert.equal(isCanonicalModel('krea-image'), false)
})
