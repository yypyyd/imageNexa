<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, jsonBody, listOf } from '../api'

const tab = ref('words')
const rows = ref([])
const loading = ref(true)
const error = ref('')
const query = ref('')
const newWord = ref('')
const importOpen = ref(false)
const importText = ref('')
const page = ref(1)
const total = ref(0)
const limit = 50
const pages = computed(() => Math.max(1, Math.ceil(total.value / limit)))

async function load() {
  loading.value = true
  const params = new URLSearchParams({ page: String(page.value), limit: String(limit) })
  if (query.value.trim()) params.set('q', query.value.trim())
  const response = await api(`/${tab.value === 'words' ? 'banned-words' : 'banned-word-hits'}?${params}`)
  if (response.ok) {
    rows.value = listOf(response.data)
    total.value = Number(response.data?.total ?? rows.value.length)
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

function switchTab(value) { tab.value = value; page.value = 1; query.value = ''; load() }
function search() { page.value = 1; load() }

async function addWord() {
  const word = newWord.value.trim()
  if (!word) return
  const response = await api('/banned-words', jsonBody('POST', { word }))
  if (response.ok) { newWord.value = ''; await load() } else error.value = response.error
}

async function importWords() {
  if (!importText.value.trim()) return
  const response = await api('/banned-words/import', jsonBody('POST', { text: importText.value }))
  if (response.ok) { importOpen.value = false; importText.value = ''; await load() } else error.value = response.error
}

async function removeWord(row) {
  if (!confirm(`确认删除违禁词「${row.word}」？`)) return
  const response = await api(`/banned-words/${encodeURIComponent(row.id)}`, { method: 'DELETE' })
  if (response.ok) await load(); else error.value = response.error
}

function fmtTime(value) {
  if (!value) return '—'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString('zh-CN', { hour12: false })
}

function keyName(row) { return row.credential?.name || row.api_credential_name || row.key_preview || 'legacy/unknown' }

onMounted(load)
</script>

<template>
  <section class="space-y-4">
    <div>
      <h2 class="text-xl font-semibold text-white/90">违禁词</h2>
      <p class="mt-1 text-xs text-white/40">同时检查文本、图片和视频请求，命中记录按 API Key 归属。</p>
    </div>

    <div class="card p-3 flex flex-wrap gap-2 items-center">
      <div class="tabs"><button :class="tab === 'words' && 'on'" @click="switchTab('words')">词库</button><button :class="tab === 'hits' && 'on'" @click="switchTab('hits')">命中记录</button></div>
      <template v-if="tab === 'words'">
        <input v-model="newWord" class="field !py-1.5 text-xs min-w-48" placeholder="输入违禁词后回车" @keyup.enter="addWord" />
        <button class="btn-primary" @click="addWord"><Icon name="plus" class="w-3.5 h-3.5" />添加</button>
        <button class="btn-soft" @click="importOpen = true">批量导入</button>
      </template>
      <input v-model="query" class="field !py-1.5 text-xs flex-1 min-w-44" :placeholder="tab === 'words' ? '搜索词库…' : '搜索命中词、模型或 request_id…'" @keyup.enter="search" />
      <button class="btn-soft" @click="search">查询</button>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div class="card overflow-hidden">
      <div v-if="loading" class="empty">加载中…</div>
      <div v-else-if="!rows.length" class="empty">{{ tab === 'words' ? '词库为空' : '暂无命中记录' }}</div>
      <div v-else-if="tab === 'words'" class="divide-y divide-white/[0.05]">
        <div v-for="row in rows" :key="row.id" class="px-4 py-3 flex gap-3 items-center hover:bg-white/[0.025]">
          <Icon name="ban" class="w-4 h-4 text-rose-300/70" />
          <span class="text-xs text-white/80 flex-1">{{ row.word }}</span>
          <span class="text-[10px] text-white/30">{{ fmtTime(row.created_at) }}</span>
          <button class="delete" @click="removeWord(row)"><Icon name="trash" class="w-3.5 h-3.5" /></button>
        </div>
      </div>
      <div v-else class="overflow-x-auto">
        <table class="w-full min-w-[850px] text-xs">
          <thead><tr class="table-head"><th>时间</th><th>命中词</th><th>API Key</th><th>类型 / 模型</th><th>Request</th><th>内容摘要</th></tr></thead>
          <tbody><tr v-for="row in rows" :key="row.id" class="table-row"><td>{{ fmtTime(row.created_at) }}</td><td><span class="hit">{{ row.word || row.matched_word }}</span></td><td>{{ keyName(row) }}</td><td><div>{{ row.kind || '—' }}</div><code class="text-[10px] text-white/35">{{ row.model || '—' }}</code></td><td><code class="text-[10px] text-white/40">{{ row.request_id || '—' }}</code></td><td class="max-w-72"><span class="line-clamp-2 text-white/45" :title="row.content_preview || row.prompt">{{ row.content_preview || row.prompt || '—' }}</span></td></tr></tbody>
        </table>
      </div>
    </div>

    <div class="flex justify-between items-center text-xs text-white/35"><span>共 {{ total }} 条</span><div class="flex gap-2 items-center"><button class="btn-soft" :disabled="page <= 1" @click="page--;load()">上一页</button><span>{{ page }} / {{ pages }}</span><button class="btn-soft" :disabled="page >= pages" @click="page++;load()">下一页</button></div></div>

    <div v-if="importOpen" class="modal-bg" @click.self="importOpen = false"><form class="modal-card" @submit.prevent="importWords"><div class="flex justify-between"><h3 class="font-semibold text-white/90">批量导入违禁词</h3><button type="button" @click="importOpen = false"><Icon name="close" class="w-4 h-4" /></button></div><textarea v-model="importText" rows="10" class="field resize-y" placeholder="每行一个词，重复项由服务端去重" /><button class="btn-primary justify-center">导入</button></form></div>
  </section>
</template>

<style scoped>
.tabs{display:flex;padding:.15rem;border-radius:.6rem;background:rgb(255 255 255 / .04)}.tabs button{padding:.35rem .7rem;border-radius:.45rem;font-size:.68rem;color:rgb(255 255 255 / .45)}.tabs button.on{background:rgb(139 92 246 / .28);color:white}.notice{border-radius:.7rem;padding:.7rem .9rem;font-size:.72rem;color:rgb(253 164 175);background:rgb(244 63 94 / .09)}.empty{padding:4rem;text-align:center;font-size:.72rem;color:rgb(255 255 255 / .3)}.delete{width:1.8rem;height:1.8rem;display:grid;place-items:center;border-radius:.45rem;color:rgb(253 164 175);background:rgb(244 63 94 / .08)}.table-head{font-size:.6rem;text-transform:uppercase;letter-spacing:.08em;color:rgb(255 255 255 / .3);border-bottom:1px solid rgb(255 255 255 / .06)}.table-head th,.table-row td{padding:.75rem 1rem;text-align:left}.table-row{border-bottom:1px solid rgb(255 255 255 / .04);color:rgb(255 255 255 / .52)}.hit{display:inline-flex;border-radius:999px;padding:.15rem .45rem;color:rgb(253 164 175);background:rgb(244 63 94 / .1)}.modal-bg{position:fixed;inset:0;z-index:50;display:grid;place-items:center;padding:1rem;background:rgb(0 0 0 / .65);backdrop-filter:blur(6px)}.modal-card{width:100%;max-width:32rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;background:#11131a;color:rgb(255 255 255 / .65);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .08)}
</style>
