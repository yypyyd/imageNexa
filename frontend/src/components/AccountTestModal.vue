<script setup>
import { computed, ref } from 'vue'
import { api, jsonBody } from '../api'
import Icon from './Icon.vue'
import SelectMenu from './SelectMenu.vue'

const props = defineProps({
  account: { type: Object, required: true },
  models: { type: Array, default: () => [] },
  initialModel: { type: String, default: '' },
})
const emit = defineEmits(['close'])

const allowedIDs = computed(() => new Set((props.account.routes || [])
  .filter((route) => route.enabled !== false)
  .map((route) => route.model_id || route.logical_model_id)))
const availableModels = computed(() => props.models.filter((model) =>
  model.enabled !== false && allowedIDs.value.has(model.id) && (model.routes || []).some((route) =>
    route.provider === props.account.provider && route.enabled !== false)))
const modelOptions = computed(() => availableModels.value.map((model) => ({
  value: model.id,
  label: `${model.id} · ${{ text: '对话', image: '图片', video: '视频' }[model.kind || model.type] || model.kind || model.type}`,
})))

const selectedModel = ref(props.initialModel)
const prompt = ref('a cute cat sitting on a desk, studio lighting')
const busy = ref(false)
const status = ref('')
const error = ref('')
const resultURL = ref('')
const resultText = ref('')

const currentModel = computed(() => availableModels.value.find((model) => model.id === selectedModel.value) || null)
const currentKind = computed(() => currentModel.value?.kind || currentModel.value?.type || '')
const accountRoute = computed(() => (currentModel.value?.routes || []).find((route) =>
  route.provider === props.account.provider && route.enabled !== false) || null)
const capabilities = computed(() => {
  const value = accountRoute.value?.capabilities
  return Array.isArray(value) ? (value[0] || {}) : (value || {})
})
const firstOf = (value, fallback = '') => Array.isArray(value) && value.length ? value[0] : fallback

async function run() {
  if (!selectedModel.value) { error.value = '请选择模型'; return }
  if (!prompt.value.trim()) { error.value = '请输入测试指令'; return }
  busy.value = true
  error.value = ''
  resultURL.value = ''
  resultText.value = ''
  status.value = currentKind.value === 'video' ? '正在生成视频，请耐心等待…' : currentKind.value === 'text' ? '正在测试对话…' : '正在生成测试图片…'
  const response = await api('/test', jsonBody('POST', {
    model: selectedModel.value,
    prompt: prompt.value.trim(),
    account_id: props.account.id,
    ratio: firstOf(capabilities.value.ratios, currentKind.value === 'video' ? '16:9' : '1:1'),
    resolution: firstOf(capabilities.value.resolutions),
    duration: firstOf(capabilities.value.durations, currentKind.value === 'video' ? '5s' : ''),
  }))
  busy.value = false
  if (!response.ok) {
    status.value = ''
    error.value = response.error
    return
  }
  resultText.value = response.data?.content || ''
  resultURL.value = response.data?.url || ''
  status.value = `测试完成${response.data?.elapsed_ms != null ? ` · ${(Number(response.data.elapsed_ms) / 1000).toFixed(1)}s` : ''}`
}
</script>

<template>
  <div class="test-modal-bg" @click.self="!busy && emit('close')">
    <div class="test-modal-card">
      <div class="flex items-start justify-between gap-3">
        <div class="min-w-0">
          <h3 class="font-semibold text-white/90">账号能力测试</h3>
          <p class="mt-1 text-[11px] text-white/45 truncate">{{ account.email || account.label || account.id }} · {{ account.provider }}</p>
        </div>
        <button type="button" class="text-white/45 hover:text-white" :disabled="busy" @click="emit('close')"><Icon name="close" class="w-4 h-4" /></button>
      </div>

      <label class="block">
        <span class="test-label">模型</span>
        <SelectMenu v-model="selectedModel" :options="modelOptions" placeholder="选择该账号已授权的模型" />
      </label>
      <p v-if="!availableModels.length" class="text-xs text-amber-300">该账号当前没有已授权的可测试模型。</p>
      <label class="block">
        <span class="test-label">测试指令</span>
        <textarea v-model="prompt" rows="3" class="field resize-none" placeholder="输入测试指令…"></textarea>
      </label>
      <button type="button" class="btn-primary w-full justify-center" :disabled="busy || !selectedModel" @click="run">
        <Icon name="test" class="w-4 h-4" />{{ busy ? '测试中…' : '开始测试' }}
      </button>
      <p v-if="status" class="text-xs text-white/60">{{ status }}</p>
      <p v-if="error" class="text-xs text-rose-300 break-all">{{ error }}</p>
      <div v-if="resultText" class="test-result text-sm whitespace-pre-wrap break-words">{{ resultText }}</div>
      <div v-if="resultURL" class="test-media">
        <video v-if="currentKind === 'video'" :src="resultURL" controls autoplay class="max-w-full max-h-[420px] object-contain" />
        <img v-else :src="resultURL" alt="账号测试结果" class="max-w-full max-h-[420px] object-contain" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.test-modal-bg{position:fixed;inset:0;z-index:60;display:grid;place-items:center;overflow-y:auto;padding:1rem;background:rgb(0 0 0 / .7);backdrop-filter:blur(6px)}
.test-modal-card{width:100%;max-width:34rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .75);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .5),inset 0 0 0 1px rgb(255 255 255 / .09)}
.test-label{display:block;margin-bottom:.4rem;font-size:.68rem;font-weight:600;color:rgb(255 255 255 / .55)}
.test-result{border-radius:.75rem;padding:1rem;color:rgb(255 255 255 / .85);background:rgb(255 255 255 / .04);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .08)}
.test-media{display:grid;min-height:14rem;place-items:center;overflow:hidden;border-radius:.75rem;background:rgb(0 0 0 / .25);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .08)}
</style>
