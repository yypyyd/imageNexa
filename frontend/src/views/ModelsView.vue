<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, jsonBody, listOf } from '../api'
import { ALL_MODELS, IMAGE_MODELS, MODEL_KIND } from '../models'

const loading = ref(true)
const saving = ref('')
const error = ref('')
const models = ref([])
const kind = ref('')
const query = ref('')
const expanded = ref(new Set())

const visibleModels = computed(() => models.value.filter((m) => MODEL_KIND[m.id]))
const unknownCount = computed(() => models.value.filter((m) => !MODEL_KIND[m.id]).length)
const missing = computed(() => ALL_MODELS.filter((id) => !models.value.some((m) => m.id === id)))
const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  return visibleModels.value.filter((m) => {
    const modelKind = m.kind || m.type || MODEL_KIND[m.id]
    if (kind.value && modelKind !== kind.value) return false
    if (q && !`${m.id} ${(m.routes || []).map((r) => `${r.provider} ${r.upstream_model}`).join(' ')}`.toLowerCase().includes(q)) return false
    return true
  })
})

const counts = computed(() => ({
  text: visibleModels.value.filter((m) => (m.kind || m.type || MODEL_KIND[m.id]) === 'text').length,
  image: visibleModels.value.filter((m) => (m.kind || m.type || MODEL_KIND[m.id]) === 'image').length,
  video: visibleModels.value.filter((m) => (m.kind || m.type || MODEL_KIND[m.id]) === 'video').length,
  routes: visibleModels.value.reduce((n, m) => n + (m.routes || []).length, 0),
}))

async function load() {
  loading.value = true
  const response = await api('/logical-models')
  if (response.ok) {
    models.value = listOf(response.data)
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

function isOpen(id) { return expanded.value.has(id) }
function toggleOpen(id) {
  const next = new Set(expanded.value)
  next.has(id) ? next.delete(id) : next.add(id)
  expanded.value = next
}

async function toggleModel(model) {
  const old = model.enabled !== false
  model.enabled = !old
  saving.value = `model:${model.id}`
  const response = await api(`/logical-models/${encodeURIComponent(model.id)}`, jsonBody('PATCH', { enabled: !old }))
  if (!response.ok) {
    model.enabled = old
    error.value = response.error
  }
  saving.value = ''
}

async function toggleRoute(model, route) {
  const old = route.enabled !== false
  route.enabled = !old
  saving.value = `route:${route.id}`
  const response = await api(`/logical-models/${encodeURIComponent(model.id)}/routes/${encodeURIComponent(route.id)}`, jsonBody('PATCH', { enabled: !old }))
  if (!response.ok) {
    route.enabled = old
    error.value = response.error
  }
  saving.value = ''
}

function capabilityText(route) {
  const c = route.capabilities || {}
  const parts = []
  const ratios = c.ratios || c.supported_ratios || []
  const resolutions = c.resolutions || c.supported_resolutions || []
  const durations = c.durations || c.supported_durations || []
  if (ratios.length) parts.push(ratios.join(' / '))
  if (resolutions.length) parts.push(resolutions.join(' / '))
  if (durations.length) parts.push(durations.join(' / '))
  if (c.max_reference_images) parts.push(`参考图 ≤ ${c.max_reference_images}`)
  if (c.max_reference_videos) parts.push(`参考视频 ≤ ${c.max_reference_videos}`)
  if (c.supports_audio_output) parts.push('音频输出')
  return parts.join(' · ') || '无额外限制'
}

onMounted(load)
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">统一模型与路由</h2>
        <p class="mt-1 text-xs text-white/40">下游只看到 canonical ID；Provider 和上游模型仅用于内部调度。</p>
      </div>
      <button class="btn-soft" :disabled="loading" @click="load"><Icon name="refresh" class="w-3.5 h-3.5" />刷新</button>
    </div>

    <div v-if="error || unknownCount || missing.length" class="space-y-2">
      <p v-if="error" class="notice bad">{{ error }}</p>
      <p v-if="unknownCount" class="notice warn">后端返回了 {{ unknownCount }} 个非闭集模型，前端已隐藏；请检查迁移数据。</p>
      <p v-if="missing.length" class="notice warn">尚未建立：<span class="font-mono">{{ missing.join(', ') }}</span></p>
    </div>

    <div class="grid grid-cols-2 md:grid-cols-4 gap-3">
      <div v-for="item in [['文本',counts.text,'text-sky-300'],['图片',counts.image,'text-indigo-300'],['视频',counts.video,'text-fuchsia-300'],['内部路由',counts.routes,'text-emerald-300']]" :key="item[0]" class="card p-4">
        <div class="text-[10px] uppercase tracking-wider text-white/40">{{ item[0] }}</div>
        <div class="mt-1 text-2xl font-semibold tabular-nums" :class="item[2]">{{ item[1] }}</div>
      </div>
    </div>

    <div class="card p-3 flex flex-wrap gap-2 items-center">
      <button v-for="option in [['','全部'],['text','文本'],['image','图片'],['video','视频']]" :key="option[0]" class="filter" :class="kind === option[0] && 'on'" @click="kind = option[0]">{{ option[1] }}</button>
      <input v-model="query" class="field !py-1.5 text-xs flex-1 min-w-52" placeholder="搜索模型 ID、Provider 或上游 ID…" />
      <span class="text-[10px] text-white/30">图片模型固定 {{ IMAGE_MODELS.length }} 个</span>
    </div>

    <div class="space-y-2">
      <article v-for="model in filtered" :key="model.id" class="card overflow-hidden">
        <div class="px-4 py-3.5 flex items-center gap-3">
          <button class="text-white/35 hover:text-white/80" @click="toggleOpen(model.id)"><span class="inline-block transition-transform" :class="isOpen(model.id) && 'rotate-90'">›</span></button>
          <span class="kind" :class="`kind-${model.kind || model.type || MODEL_KIND[model.id]}`">{{ model.kind || model.type || MODEL_KIND[model.id] }}</span>
          <div class="min-w-0 flex-1">
            <div class="font-mono text-xs text-white/90 truncate">{{ model.id }}</div>
            <div class="mt-1 text-[10px] text-white/35">{{ (model.routes || []).filter((r) => r.enabled !== false).length }} / {{ (model.routes || []).length }} 路由已启用</div>
          </div>
          <button class="switch" :class="model.enabled !== false && 'on'" :disabled="saving === `model:${model.id}`" @click="toggleModel(model)"><span></span></button>
        </div>

        <div v-if="isOpen(model.id)" class="border-t border-white/[0.06] bg-white/[0.015]">
          <div v-if="!(model.routes || []).length" class="px-12 py-6 text-xs text-amber-300/80">没有可用路由，模型不应对外展示。</div>
          <div v-for="route in model.routes || []" :key="route.id" class="px-5 md:px-12 py-3 border-b border-white/[0.04] last:border-0 grid md:grid-cols-[9rem_minmax(0,1fr)_8rem_8rem_auto] gap-2 md:gap-4 items-center">
            <div><span class="provider">{{ route.provider }}</span></div>
            <div class="min-w-0">
              <div class="font-mono text-[11px] text-white/70 truncate" :title="route.upstream_model">{{ route.upstream_model || '—' }}</div>
              <div class="mt-1 text-[10px] text-white/35 truncate" :title="capabilityText(route)">{{ capabilityText(route) }}</div>
            </div>
            <div class="text-[10px] text-white/40"><span class="text-white/75">{{ route.healthy_accounts ?? 0 }}</span> / {{ route.account_count ?? 0 }} 账号</div>
            <div class="text-[10px] text-white/40">优先级 <span class="text-white/75">{{ route.priority ?? 0 }}</span></div>
            <button class="switch" :class="route.enabled !== false && 'on'" :disabled="saving === `route:${route.id}`" @click="toggleRoute(model, route)"><span></span></button>
          </div>
        </div>
      </article>
      <div v-if="!loading && !filtered.length" class="card py-16 text-center text-xs text-white/35">没有匹配的 canonical 模型</div>
    </div>
  </section>
</template>

<style scoped>
.notice { border-radius: .7rem; padding: .65rem .85rem; font-size: .72rem; line-height: 1.45; }
.notice.bad { color: rgb(253 164 175); background: rgb(244 63 94 / .09); box-shadow: inset 0 0 0 1px rgb(244 63 94 / .22); }
.notice.warn { color: rgb(252 211 77); background: rgb(245 158 11 / .08); box-shadow: inset 0 0 0 1px rgb(245 158 11 / .2); }
.filter { padding: .38rem .7rem; border-radius: .55rem; font-size: .7rem; color: rgb(255 255 255 / .55); background: rgb(255 255 255 / .04); }
.filter.on { color: white; background: rgb(139 92 246 / .32); box-shadow: inset 0 0 0 1px rgb(167 139 250 / .4); }
.kind,.provider { display: inline-flex; border-radius: 999px; padding: .15rem .5rem; font: 600 .62rem ui-monospace,SFMono-Regular,monospace; background: rgb(255 255 255 / .055); color: rgb(255 255 255 / .55); box-shadow: inset 0 0 0 1px rgb(255 255 255 / .07); }
.kind-text { color: rgb(125 211 252); background: rgb(14 165 233 / .1); }
.kind-image { color: rgb(165 180 252); background: rgb(99 102 241 / .1); }
.kind-video { color: rgb(240 171 252); background: rgb(217 70 239 / .1); }
.switch { position: relative; width: 2.2rem; height: 1.25rem; flex: none; border-radius: 999px; background: rgb(255 255 255 / .12); transition: background .15s; }
.switch span { position: absolute; width: .95rem; height: .95rem; left: .15rem; top: .15rem; border-radius: 999px; background: white; transition: transform .15s; }
.switch.on { background: rgb(16 185 129 / .7); }
.switch.on span { transform: translateX(.95rem); }
</style>
