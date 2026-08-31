<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import MediaLightbox from '../components/MediaLightbox.vue'
import { api, apiURL, listOf } from '../api'
import { ALL_MODELS } from '../models'

const tab = ref('logs')
const rows = ref([])
const credentials = ref([])
const loading = ref(true)
const error = ref('')
const total = ref(0)
const page = ref(1)
const limit = 30
const credentialId = ref('')
const model = ref('')
const status = ref('')
const query = ref('')
const preview = ref(null)

const pages = computed(() => Math.max(1, Math.ceil(total.value / limit)))

function buildURL() {
  const params = new URLSearchParams({ page: String(page.value), limit: String(limit) })
  if (credentialId.value) params.set('credential_id', credentialId.value)
  if (model.value) params.set('model', model.value)
  if (status.value && tab.value === 'logs') params.set('status', status.value)
  if (query.value.trim()) params.set('q', query.value.trim())
  return `/${tab.value}?${params}`
}

async function load() {
  loading.value = true
  const response = await api(buildURL())
  if (response.ok) {
    rows.value = listOf(response.data)
    total.value = Number(response.data?.total ?? rows.value.length)
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

async function loadCredentials() {
  const response = await api('/api-credentials?limit=500')
  if (response.ok) credentials.value = listOf(response.data)
}

function switchTab(value) {
  tab.value = value
  page.value = 1
  load()
}

function search() { page.value = 1; load() }
function go(delta) { page.value = Math.max(1, Math.min(pages.value, page.value + delta)); load() }

function fmtTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function fmtMs(value) {
  const n = Number(value || 0)
  if (!n) return '—'
  return n >= 1000 ? `${(n / 1000).toFixed(2)}s` : `${n}ms`
}

function fmtSize(value) {
  const n = Number(value || 0)
  if (!n) return '—'
  if (n > 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  if (n > 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

function keyName(row) {
  return row.credential?.name || row.api_credential_name || row.credential_name || row.key_preview || 'legacy/unknown'
}

function mediaURL(row, thumbnail = false) {
  const value = thumbnail ? (row.thumbnail_url || row.content_url) : row.content_url
  if (!value) return ''
  if (/^https?:\/\//.test(value)) return value
  if (value.startsWith('/')) return apiURL(value)
  return apiURL(`/images/${value}`)
}

onMounted(() => { loadCredentials(); load() })
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">日志与成品</h2>
        <p class="mt-1 text-xs text-white/40">按 API Key 归属追踪请求，并查看图片、视频等历史成品。</p>
      </div>
      <button class="btn-soft" @click="load"><Icon name="refresh" class="w-3.5 h-3.5" />刷新</button>
    </div>

    <div class="card p-3 flex flex-wrap gap-2 items-center">
      <div class="tabs"><button :class="tab === 'logs' && 'on'" @click="switchTab('logs')">调用日志</button><button :class="tab === 'artifacts' && 'on'" @click="switchTab('artifacts')">生成成品</button></div>
      <select v-model="credentialId" class="field !py-1.5 text-xs w-44" @change="search"><option value="">全部 API Key</option><option v-for="key in credentials" :key="key.id" :value="key.id">{{ key.name }} · {{ key.key_preview }}</option></select>
      <select v-model="model" class="field !py-1.5 text-xs w-48" @change="search"><option value="">全部模型</option><option v-for="id in ALL_MODELS" :key="id" :value="id">{{ id }}</option></select>
      <select v-if="tab === 'logs'" v-model="status" class="field !py-1.5 text-xs w-32" @change="search"><option value="">全部状态</option><option value="succeeded">成功</option><option value="failed">失败</option><option value="processing">处理中</option><option value="unknown">提交未知</option></select>
      <input v-model="query" class="field !py-1.5 text-xs flex-1 min-w-44" placeholder="搜索 request_id、提示词或错误…" @keyup.enter="search" />
      <button class="btn-soft" @click="search">查询</button>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div v-if="tab === 'logs'" class="card overflow-hidden">
      <div v-if="loading" class="empty">加载中…</div>
      <div v-else-if="!rows.length" class="empty">暂无调用日志</div>
      <div v-else class="overflow-x-auto">
        <table class="w-full min-w-[1050px] text-xs">
          <thead><tr class="table-head"><th>时间 / Request</th><th>API Key</th><th>模型</th><th>内部调度</th><th>状态</th><th>耗时</th><th>结果</th></tr></thead>
          <tbody><tr v-for="row in rows" :key="row.id || row.request_id" class="table-row">
            <td><div class="text-white/60">{{ fmtTime(row.created_at) }}</div><code class="block mt-1 text-[10px] text-white/30 max-w-48 truncate" :title="row.request_id">{{ row.request_id || row.id }}</code></td>
            <td><div class="text-white/75">{{ keyName(row) }}</div><code class="text-[10px] text-white/30">{{ row.credential?.key_preview || row.key_preview }}</code></td>
            <td><code class="text-[11px] text-white/75">{{ row.model || '—' }}</code><div class="mt-1 text-[10px] text-white/30">{{ row.kind || '—' }}</div></td>
            <td><div class="text-[10px] text-white/50">{{ row.provider || '—' }}</div><div class="mt-1 text-[10px] text-white/30">{{ row.account_label || row.account_id || '—' }}</div></td>
            <td><span class="status" :class="`status-${row.status}`">{{ row.status || '—' }}</span></td>
            <td class="tabular-nums text-white/45">{{ fmtMs(row.duration_ms || row.elapsed_ms) }}</td>
            <td class="max-w-56"><span v-if="row.error" class="line-clamp-2 text-[10px] text-rose-300" :title="row.error">{{ row.error }}</span><span v-else class="text-emerald-300/70">完成</span></td>
          </tr></tbody>
        </table>
      </div>
    </div>

    <div v-else>
      <div v-if="loading" class="card empty">加载中…</div>
      <div v-else-if="!rows.length" class="card empty">暂无生成成品</div>
      <div v-else class="grid sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4 gap-3">
        <article v-for="row in rows" :key="row.id" class="card overflow-hidden group">
          <button class="media" @click="preview = row">
            <div v-if="mediaURL(row,true)" class="absolute inset-0 bg-center bg-cover transition-transform duration-300 group-hover:scale-[1.03]" :style="{backgroundImage:`url(${JSON.stringify(mediaURL(row,true))})`}"></div>
            <div v-else class="absolute inset-0 grid place-items-center text-white/20"><Icon :name="row.kind === 'video' ? 'video' : 'files'" class="w-8 h-8" /></div>
            <span class="absolute left-2.5 top-2.5 kind">{{ row.kind || row.mime_type || '成品' }}</span>
          </button>
          <div class="p-3">
            <div class="flex gap-2 items-center"><code class="text-[11px] text-white/80 truncate flex-1">{{ row.model }}</code><span class="text-[9px] text-white/30">{{ fmtSize(row.size_bytes) }}</span></div>
            <p class="mt-2 text-[10px] leading-4 text-white/40 line-clamp-2 min-h-8">{{ row.prompt || '无提示词记录' }}</p>
            <div class="mt-2 flex justify-between gap-2 text-[9px] text-white/25"><span class="truncate">{{ keyName(row) }}</span><time>{{ fmtTime(row.created_at) }}</time></div>
          </div>
        </article>
      </div>
    </div>

    <div class="flex items-center justify-between text-xs text-white/35">
      <span>共 {{ total }} 条</span><div class="flex items-center gap-2"><button class="btn-soft" :disabled="page <= 1" @click="go(-1)">上一页</button><span>{{ page }} / {{ pages }}</span><button class="btn-soft" :disabled="page >= pages" @click="go(1)">下一页</button></div>
    </div>

    <MediaLightbox v-if="preview && mediaURL(preview)" :src="mediaURL(preview)" :kind="preview.kind === 'video' || String(preview.mime_type).startsWith('video/') ? 'video' : 'image'" :prompt="preview.prompt || ''" :meta="[preview.model,keyName(preview)].filter(Boolean).join(' · ')" :meta-sub="fmtTime(preview.created_at)" @close="preview = null" />
  </section>
</template>

<style scoped>
.tabs{display:flex;padding:.15rem;border-radius:.6rem;background:rgb(255 255 255 / .04);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .06)}.tabs button{padding:.35rem .65rem;border-radius:.45rem;font-size:.68rem;color:rgb(255 255 255 / .45)}.tabs button.on{background:rgb(139 92 246 / .28);color:white}.notice{border-radius:.7rem;padding:.7rem .9rem;font-size:.72rem;color:rgb(253 164 175);background:rgb(244 63 94 / .09)}.empty{padding:4rem;text-align:center;font-size:.72rem;color:rgb(255 255 255 / .3)}
.table-head{border-bottom:1px solid rgb(255 255 255 / .06);font-size:.6rem;text-transform:uppercase;letter-spacing:.1em;color:rgb(255 255 255 / .32)}.table-head th,.table-row td{padding:.75rem 1rem;text-align:left;vertical-align:middle}.table-row{border-bottom:1px solid rgb(255 255 255 / .04)}.table-row:hover{background:rgb(255 255 255 / .025)}.status,.kind{display:inline-flex;border-radius:999px;padding:.15rem .45rem;font-size:.6rem;color:rgb(255 255 255 / .5);background:rgb(255 255 255 / .07)}.status-succeeded,.status-success{color:rgb(110 231 183);background:rgb(16 185 129 / .1)}.status-failed{color:rgb(253 164 175);background:rgb(244 63 94 / .1)}.status-processing{color:rgb(125 211 252);background:rgb(14 165 233 / .1)}.status-unknown{color:rgb(252 211 77);background:rgb(245 158 11 / .1)}
.media{display:block;position:relative;width:100%;aspect-ratio:16/10;background:rgb(0 0 0 / .25);overflow:hidden}.media::after{content:"";position:absolute;inset:0;background:linear-gradient(to top,rgb(0 0 0 / .35),transparent 45%)}.media .kind{z-index:1;color:white;background:rgb(0 0 0 / .4);backdrop-filter:blur(8px)}
</style>
