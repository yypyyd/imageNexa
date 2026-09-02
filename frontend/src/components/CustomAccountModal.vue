<script setup>
import { computed, ref } from 'vue'
import { api, jsonBody } from '../api'
import Icon from './Icon.vue'

const props = defineProps({
  account: { type: Object, default: null },
  models: { type: Array, default: () => [] },
})
const emit = defineEmits(['close', 'saved'])

const isEdit = computed(() => !!props.account?.id)
const label = ref(props.account?.label || props.account?.email || '')
const baseURL = ref(props.account?.base_url || '')
const apiKey = ref('')
const selected = ref(String(props.account?.models || '')
  .split(',').map((value) => value.trim()).filter(Boolean))
const weight = ref(Number(props.account?.weight) || 0)
const maxConcurrency = ref(Number(props.account?.max_concurrency) || 1)
const busy = ref(false)
const error = ref('')

const canonicalModels = computed(() => props.models.filter((model) => model?.id && model.enabled !== false))

function toggleModel(id) {
  selected.value = selected.value.includes(id)
    ? selected.value.filter((value) => value !== id)
    : [...selected.value, id]
}

async function submit() {
  const url = baseURL.value.trim()
  const key = apiKey.value.trim()
  if (!url) { error.value = '请填写公开 HTTPS Base URL'; return }
  if (!isEdit.value && !key) { error.value = '请填写 API Key'; return }
  if (!selected.value.length) { error.value = '至少选择一个 canonical 模型'; return }
  const concurrency = Number(maxConcurrency.value)
  const routeWeight = Number(weight.value)
  if (!Number.isInteger(concurrency) || concurrency < 0 || concurrency > 1000) {
    error.value = '最大并发必须是 0 到 1000 的整数'
    return
  }
  if (!Number.isInteger(routeWeight) || routeWeight < -1000 || routeWeight > 1000) {
    error.value = '权重必须是 -1000 到 1000 的整数'
    return
  }
  busy.value = true
  error.value = ''
  const response = await api('/accounts/import', jsonBody('POST', {
    id: isEdit.value ? props.account.id : '',
    provider: 'custom',
    label: label.value.trim(),
    credential: {
      base_url: url,
      api_key: key,
      models: selected.value,
      weight: routeWeight,
      max_concurrency: concurrency,
    },
  }))
  busy.value = false
  if (!response.ok) { error.value = response.error; return }
  emit('saved', response.data?.data || response.data)
}
</script>

<template>
  <div class="custom-modal-bg" @click.self="!busy && emit('close')">
    <form class="custom-modal-card" @submit.prevent="submit">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3 class="font-semibold text-white/90">{{ isEdit ? '编辑自定义上游' : '添加自定义上游' }}</h3>
          <p class="mt-1 text-[11px] leading-5 text-white/45">绑定现有 canonical 模型闭集；调用固定走该上游，不会把 Key 回显到页面。</p>
        </div>
        <button type="button" class="text-white/45 hover:text-white" :disabled="busy" @click="emit('close')"><Icon name="close" class="w-4 h-4" /></button>
      </div>

      <label class="block"><span class="custom-label">备注名</span><input v-model="label" class="field" placeholder="例如：内部中转 / 图像节点 A" /></label>
      <label class="block"><span class="custom-label">Base URL <b>*</b></span><input v-model="baseURL" class="field font-mono text-xs" placeholder="https://api.example.com（无需 /v1 结尾）" /></label>
      <label class="block">
        <span class="custom-label">API Key <b v-if="!isEdit">*</b><em v-else>留空保持原 Key</em></span>
        <input v-model="apiKey" type="password" autocomplete="new-password" class="field font-mono text-xs" :placeholder="isEdit ? '留空不修改' : 'sk-…'" />
      </label>

      <div>
        <div class="mb-2 flex items-center justify-between gap-2"><span class="custom-label !mb-0">支持的 canonical 模型 <b>*</b></span><small class="text-white/35">已选 {{ selected.length }}</small></div>
        <div v-if="canonicalModels.length" class="model-grid">
          <button v-for="model in canonicalModels" :key="model.id" type="button" :class="selected.includes(model.id) && 'on'" @click="toggleModel(model.id)">
            <span :class="`dot-${model.kind || model.type}`"></span><code>{{ model.id }}</code>
          </button>
        </div>
        <p v-else class="empty-models">当前没有可绑定的 canonical 模型。</p>
      </div>

      <div class="grid grid-cols-2 gap-3">
        <label><span class="custom-label">权重（高的优先）</span><input v-model.number="weight" type="number" min="-1000" max="1000" class="field" /></label>
        <label><span class="custom-label">最大并发</span><input v-model.number="maxConcurrency" type="number" min="0" max="1000" class="field" /><small class="mt-1 block text-[10px] text-white/35">0 = 使用 Provider 默认值</small></label>
      </div>

      <p v-if="error" class="text-xs text-rose-300 break-all">{{ error }}</p>
      <button class="btn-primary w-full justify-center" :disabled="busy || !canonicalModels.length">{{ busy ? '保存中…' : (isEdit ? '保存修改' : '添加上游') }}</button>
    </form>
  </div>
</template>

<style scoped>
.custom-modal-bg{position:fixed;inset:0;z-index:65;display:grid;place-items:center;overflow-y:auto;padding:1rem;background:rgb(0 0 0 / .7);backdrop-filter:blur(6px)}
.custom-modal-card{width:100%;max-width:38rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .75);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .5),inset 0 0 0 1px rgb(255 255 255 / .09)}
.custom-label{display:block;margin-bottom:.4rem;color:rgb(255 255 255 / .55);font-size:.68rem;font-weight:600}.custom-label b{color:rgb(253 164 175)}.custom-label em{margin-left:.35rem;color:rgb(255 255 255 / .3);font-style:normal;font-weight:400}
.model-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:.4rem;max-height:14rem;overflow-y:auto;border-radius:.7rem;padding:.55rem;background:rgb(255 255 255 / .025);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .07)}
.model-grid button{display:flex;align-items:center;min-width:0;gap:.45rem;border-radius:.5rem;padding:.48rem .58rem;color:rgb(255 255 255 / .55);background:rgb(255 255 255 / .035);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .06);text-align:left}.model-grid button:hover{color:white;background:rgb(255 255 255 / .07)}.model-grid button.on{color:rgb(196 181 253);background:rgb(139 92 246 / .14);box-shadow:inset 0 0 0 1px rgb(167 139 250 / .35)}
.model-grid code{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:.62rem}.model-grid span{width:.38rem;height:.38rem;flex:none;border-radius:999px;background:rgb(148 163 184)}.model-grid .dot-text{background:rgb(56 189 248)}.model-grid .dot-image{background:rgb(129 140 248)}.model-grid .dot-video{background:rgb(232 121 249)}
.empty-models{border-radius:.7rem;padding:.8rem;color:rgb(252 211 77 / .8);background:rgb(245 158 11 / .07);font-size:.7rem}
@media(max-width:640px){.model-grid{grid-template-columns:1fr}}
</style>
