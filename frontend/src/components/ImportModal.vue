<script setup>
import { ref, computed } from 'vue'
import { api, jsonBody } from '../api'
import { parseImportFile, parseImportInput, uniqueImportItems } from '../utils/import'
import Icon from './Icon.vue'

const emit = defineEmits(['close', 'imported'])

const input = ref('')
const weight = ref(0)
const status = ref('')
const isError = ref(false)
const submitting = ref(false)
const fileInput = ref(null)
const fileItems = ref([])
const fileNames = ref([])

// type → token pool (for the post-import weight PATCH).
const TYPE_POOL = { openai: 'chatgpt', adobe: 'adobe', byteplus: 'byteplus', runway: 'runway', leonardo: 'leonardo', krea: 'krea', imagine: 'imagine', grok: 'grok', oreate: 'oreate' }

// Live preview of what the parser would extract — updates as the user types
// so they can see whether their paste was understood before clicking import.
const detected = computed(() => {
  const items = uniqueImportItems([...parseImportInput(input.value), ...fileItems.value])
  const openai = items.filter((x) => x.type === 'openai').length
  const adobe = items.filter((x) => x.type === 'adobe').length
  const byteplus = items.filter((x) => x.type === 'byteplus').length
  const runway = items.filter((x) => x.type === 'runway').length
  const leonardo = items.filter((x) => x.type === 'leonardo').length
  const krea = items.filter((x) => x.type === 'krea').length
  const imagine = items.filter((x) => x.type === 'imagine').length
  const grok = items.filter((x) => x.type === 'grok').length
  const oreate = items.filter((x) => x.type === 'oreate').length
  return { total: items.length, openai, adobe, byteplus, runway, leonardo, krea, imagine, grok, oreate }
})

const importItems = computed(() => uniqueImportItems([...parseImportInput(input.value), ...fileItems.value]))

function setStatus(text, err = false) {
  status.value = text || ''
  isError.value = err
}

async function doSmartImport() {
  const items = importItems.value
  if (!items.length) {
    setStatus('未识别到任何 Cookie 或 JWT', true)
    return
  }
  submitting.value = true
  let ok = 0, fail = 0
  const errs = []
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    try {
      const r = it.type === 'openai'
        ? await api('/tokens/import-chatgpt-token', jsonBody('POST', { access_token: it.value }))
        : it.type === 'oreate'
          ? await api('/tokens/import-oreate-account', jsonBody('POST', {
            cookie: it.value,
            email: it.meta?.email || '',
            ouid: it.meta?.ouid || '',
            user_agent: it.meta?.user_agent || '',
            reg_ts: Number(it.meta?.reg_ts || 0),
            vip: it.meta?.vip || '0',
          }))
        : it.type === 'grok'
          ? await api('/tokens/import-grok-token', jsonBody('POST', { access_token: it.value }))
        : it.type === 'runway'
          ? await api('/tokens/import-runway-token', jsonBody('POST', { access_token: it.value }))
          : it.type === 'byteplus'
            ? await api('/tokens/import-byteplus-cookie', jsonBody('POST', { cookie: it.value }))
          : it.type === 'leonardo'
            ? await api('/tokens/import-leonardo-cookie', jsonBody('POST', { cookie: it.value }))
            : it.type === 'krea'
              ? await api('/tokens/import-krea-cookie', jsonBody('POST', { cookie: it.value }))
              : it.type === 'imagine'
                ? await api('/tokens/import-imagine-token', jsonBody('POST', { value: it.value }))
                : await api('/tokens/import-adobe-cookie', jsonBody('POST', { cookie: it.value }))
      if (r.ok) {
        ok++
        // Apply the chosen weight to the freshly-imported account (best-effort).
        const w = Number(weight.value) || 0
        const pool = TYPE_POOL[it.type]
        if (w !== 0 && pool && r.data?.id) {
          try { await api(`/tokens/${pool}/${r.data.id}`, jsonBody('PATCH', { weight: w })) } catch (_) {}
        }
      } else { fail++; errs.push(`${it.type}: ${r.data?.detail || r.status}`) }
    } catch (e) {
      fail++; errs.push(`${it.type}: ${e}`)
    }
  }
  submitting.value = false
  // Quota isn't checked here — the server probes each token off-thread and the
  // account list flips pending → active/dead on its own.
  if (fail === 0) {
    // Close immediately on success — the account lands in the table right away
    // and the server backfills quota/email off-thread (pending → active/dead).
    emit('imported')
    emit('close')
  } else {
    setStatus(`成功 ${ok} · 失败 ${fail} · ${errs.slice(0, 3).join(' | ')}`, true)
    emit('imported')
  }
}

async function selectFiles(event) {
  const files = [...(event.target.files || [])]
  event.target.value = ''
  if (!files.length) return
  try {
    const parsed = []
    for (const file of files) parsed.push(...await parseImportFile(file))
    fileItems.value = uniqueImportItems(parsed)
    fileNames.value = files.map((file) => file.name)
    setStatus(`已读取 ${files.length} 个文件，识别到 ${fileItems.value.length} 个账号`)
  } catch (error) {
    fileItems.value = []
    fileNames.value = []
    setStatus(error?.message || String(error), true)
  }
}

function clearFiles() {
  fileItems.value = []
  fileNames.value = []
  setStatus('')
}
</script>

<template>
  <div class="fixed inset-0 z-50 bg-slate-900/40 backdrop-blur-sm flex items-start justify-center overflow-y-auto p-4"
       @click.self="emit('close')">
    <div class="card !shadow-xl mt-14 mb-14 w-full max-w-2xl">
      <div class="px-5 py-4 border-b border-slate-100 flex items-center justify-between">
        <h2 class="text-sm font-semibold">导入账号</h2>
        <button @click="emit('close')" class="text-slate-400 hover:text-slate-700 transition-colors">
          <Icon name="close" class="w-5 h-5" />
        </button>
      </div>

      <div class="p-5">
        <p class="text-xs text-slate-500 mb-3 leading-relaxed">自动识别：
          <strong class="text-slate-700">CPA JSON / ZIP</strong>、
          <strong class="text-slate-700">Sub2API 聚合 JSON</strong>、
          <strong class="text-slate-700">Adobe Cookie 字符串</strong>(<code class="px-1 bg-slate-100 rounded">k=v; k=v; ...</code>)、
          <strong class="text-slate-700">Cookie JSON 对象</strong>、
          <strong class="text-slate-700">Cookie 数组</strong>(多 Adobe 批量)、
          <strong class="text-slate-700">ChatGPT JWT</strong>(<code class="px-1 bg-slate-100 rounded">eyJhbGciOi...</code>)、
          <strong class="text-slate-700">Runway JWT</strong>(自动与 ChatGPT 区分)、
          <strong class="text-slate-700">BytePlus Cookie</strong>(含 <code class="px-1 bg-slate-100 rounded">csrfToken</code>)、
          <strong class="text-slate-700">Leonardo Cookie</strong>(含 better-auth)、
          <strong class="text-slate-700">Krea Cookie</strong>(含 sb-superb-auth)、
          <strong class="text-slate-700">Imagine Token</strong>(<code class="px-1 bg-slate-100 rounded">{"token","refreshToken","email","parentId"}</code>)、
          <strong class="text-slate-700">Grok SSO</strong>(grok.com 的 <code class="px-1 bg-slate-100 rounded">sso</code> 值或 grok2api JSON,仅含 session_id,自动与 ChatGPT/Runway 区分)、
          <strong class="text-slate-700">OreateAI 账号 JSON</strong>(只导入 Cookie，不保存密码)、
          <strong class="text-slate-700">多个 JWT</strong>(换行分隔)。
          全粘进来即可，无需任何前缀。
        </p>
        <input ref="fileInput" type="file" accept=".json,.zip,application/json,application/zip" multiple class="hidden" @change="selectFiles" />
        <div class="mb-3 rounded-lg border border-dashed border-slate-300 bg-slate-50/60 px-3 py-2.5 flex items-center gap-2">
          <button type="button" class="btn-soft shrink-0" @click="fileInput?.click()">选择 CPA / Sub 文件</button>
          <span v-if="fileNames.length" class="text-xs text-emerald-700 truncate">
            {{ fileNames.length === 1 ? fileNames[0] : `${fileNames.length} 个文件` }} · {{ fileItems.length }} 个账号
          </span>
          <span v-else class="text-xs text-slate-400 truncate">支持 .json、CPA 批量 .zip，可多选</span>
          <button v-if="fileNames.length" type="button" class="ml-auto text-xs text-slate-400 hover:text-rose-600" @click="clearFiles">清除</button>
        </div>
        <textarea v-model="input" rows="10"
                  class="field font-mono text-xs resize-none"
                  placeholder="也可直接粘贴 CPA / Sub2API JSON、Cookie 字符串或 JWT，自动识别"></textarea>
        <div v-if="input.trim() || fileItems.length" class="mt-2 flex items-center gap-2 text-xs flex-wrap">
          <template v-if="detected.total">
            <span class="text-emerald-600">✓ 识别到 <strong class="tabular-nums">{{ detected.total }}</strong> 个账号</span>
            <span v-if="detected.openai" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-emerald-700 bg-emerald-50 ring-1 ring-emerald-200">
              OpenAI · <span class="tabular-nums">{{ detected.openai }}</span>
            </span>
            <span v-if="detected.adobe" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-rose-700 bg-rose-50 ring-1 ring-rose-200">
              Adobe · <span class="tabular-nums">{{ detected.adobe }}</span>
            </span>
            <span v-if="detected.byteplus" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-indigo-700 bg-indigo-50 ring-1 ring-indigo-200">
              BytePlus · <span class="tabular-nums">{{ detected.byteplus }}</span>
            </span>
            <span v-if="detected.runway" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-violet-700 bg-violet-50 ring-1 ring-violet-200">
              Runway · <span class="tabular-nums">{{ detected.runway }}</span>
            </span>
            <span v-if="detected.leonardo" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-amber-700 bg-amber-50 ring-1 ring-amber-200">
              Leonardo · <span class="tabular-nums">{{ detected.leonardo }}</span>
            </span>
            <span v-if="detected.krea" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-sky-700 bg-sky-50 ring-1 ring-sky-200">
              Krea · <span class="tabular-nums">{{ detected.krea }}</span>
            </span>
            <span v-if="detected.imagine" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-teal-700 bg-teal-50 ring-1 ring-teal-200">
              Imagine · <span class="tabular-nums">{{ detected.imagine }}</span>
            </span>
            <span v-if="detected.grok" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-slate-700 bg-slate-100 ring-1 ring-slate-300">
              Grok · <span class="tabular-nums">{{ detected.grok }}</span>
            </span>
            <span v-if="detected.oreate" class="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-cyan-700 bg-cyan-50 ring-1 ring-cyan-200">
              OreateAI · <span class="tabular-nums">{{ detected.oreate }}</span>
            </span>
          </template>
          <span v-else class="text-rose-600">未识别到任何 Cookie 或 JWT</span>
        </div>
        <div class="mt-3 flex items-center gap-2">
          <label class="text-xs text-slate-500 whitespace-nowrap">权重(本批账号,高的优先)</label>
          <input v-model.number="weight" type="number" class="field !w-24" placeholder="0" />
        </div>
        <button @click="doSmartImport" :disabled="submitting || !detected.total" class="btn-primary w-full mt-3">
          {{ submitting ? '导入中…' : (detected.total ? `识别并导入 (${detected.total})` : '识别并导入') }}
        </button>
        <p v-if="status" class="text-xs mt-2" :class="isError ? 'text-rose-600' : 'text-emerald-600'">{{ status }}</p>
      </div>
    </div>
  </div>
</template>
