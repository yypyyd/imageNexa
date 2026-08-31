import assert from 'node:assert/strict'
import test from 'node:test'
import { ALL_MODELS, IMAGE_MODELS, MODEL_KIND, TEXT_MODELS, VIDEO_MODELS, isCanonicalModel } from './models.js'

test('canonical model catalog is closed and unique', () => {
  assert.equal(TEXT_MODELS.length, 4)
  assert.equal(IMAGE_MODELS.length, 6)
  assert.equal(VIDEO_MODELS.length, 14)
  assert.equal(new Set(ALL_MODELS).size, 24)
  assert.equal(Object.keys(MODEL_KIND).length, 24)
})

test('only the six approved image ids are accepted', () => {
  assert.deepEqual(IMAGE_MODELS, [
    'gpt-image-2',
    'seedream-5.0-pro',
    'seedream-5.0-lite',
    'nano-banana-2',
    'nano-banana-pro',
    'grok-imagine-image',
  ])
  assert.equal(isCanonicalModel('lumina-image'), false)
  assert.equal(isCanonicalModel('firefly-image-5'), false)
  assert.equal(isCanonicalModel('krea-image'), false)
})
