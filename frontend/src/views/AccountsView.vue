<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import SelectMenu from '../components/SelectMenu.vue'
import { api, jsonBody, listOf } from '../api'
import { parseCredentialInput } from '../credential'

const ALLOWED_PROVIDERS = ['chatgpt', 'byteplus', 'adobe', 'runway', 'grok', 'oreate', 'custom']
const PROVIDER_OPTIONS = ALLOWED_PROVIDERS.map((value) => ({ value, label: value === 'chatgpt' ? 'ChatGPT' : value === 'oreate' ? 'OreateAI' : value[0].toUpperCase() + value.slice(1) }))

const accounts = ref([])
const loading = ref(true)
const error = ref('')
const total = ref(0)
const page = ref(1)
const limit = 100
const query = ref('')
const provider = ref('')
const status = ref('')
const expanded = ref(new Set())
const busy = ref('')
const importing = ref(false)
const importForm = reactive({ provider: 'byteplus', label: '', credential: '' })

const filtered = computed(() => {
  const q = query.value.trim().toLowerCase()
  return accounts.value.filter((a) => {
    if (!ALLOWED_PROVIDERS.includes(String(a.provider || '').toLowerCase())) return false
    if (provider.value && a.provider !== provider.value) return false
    if (status.value && a.status !== status.value) return false
    if (q && !`${a.label || ''} ${a.email || ''} ${a.id || ''} ${a.provider || ''}`.toLowerCase().includes(q)) return false
    return true
  })
})

const pages = computed(() => Math.max(1, Math.ceil(total.value / limit)))

const stats = computed(() => ({
  total: total.value,
  active: accounts.value.filter((a) => ALLOWED_PROVIDERS.includes(a.provider) && ['active', 'enabled', 'healthy'].includes(a.status)).length,
  cooldown: accounts.value.filter((a) => ['cooldown', 'quota', 'pending'].includes(a.status)).length,
  disabled: accounts.value.filter((a) => ['disabled', 'auth_error'].includes(a.status)).length,
}))

function listURL() {
  const params = new URLSearchParams({ page: String(page.value), limit: String(limit) })
  if (provider.value) params.set('provider', provider.value)
  if (status.value) params.set('status', status.value)
  if (query.value.trim()) params.set('q', query.value.trim())
  return `/accounts?${params}`
}

async function load() {
  loading.value = true
  const response = await api(listURL())
  if (response.ok) {
    accounts.value = listOf(response.data)
    total.value = Number(response.data?.total ?? accounts.value.length)
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

function search() { page.value = 1; load() }
function go(delta) { page.value = Math.max(1, Math.min(pages.value, page.value + delta)); load() }

function toggleOpen(id) {
  const next = new Set(expanded.value)
  next.has(id) ? next.delete(id) : next.add(id)
  expanded.value = next
}

function quotaPercent(bucket) {
  const total = Number(bucket.total || 0)
  if (!total) return 0
  return Math.max(0, Math.min(100, Number(bucket.remaining || 0) / total * 100))
}

function quotaValue(value, unit) {
  if (value === null || value === undefined) return '—'
  return `${Number(value).toLocaleString('zh-CN')} ${unit || ''}`.trim()
}

function fmtTime(value) {
  if (!value) return '—'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString('zh-CN', { hour12: false })
}

async function patchAccount(account, patch, key) {
  busy.value = `${account.id}:${key}`
  const response = await api(`/accounts/${encodeURIComponent(account.id)}`, jsonBody('PATCH', patch))
  if (!response.ok) error.value = response.error
  else Object.assign(account, response.data?.data || response.data || patch)
  busy.value = ''
}

async function toggleAccount(account) {
  const active = !['disabled', 'auth_error'].includes(account.status)
  await patchAccount(account, { status: active ? 'disabled' : 'active' }, 'status')
}

async function refreshQuota(account) {
  busy.value = `${account.id}:quota`
  const response = await api(`/accounts/${encodeURIComponent(account.id)}/refresh-quota`, jsonBody('POST', {}))
  if (response.ok) {
    await load()
  } else error.value = response.error
  busy.value = ''
}

async function removeAccount(account) {
  if (!confirm(`确认删除 ${account.label || account.email || account.id}？已产生的日志仍会保留快照。`)) return
  const response = await api(`/accounts/${encodeURIComponent(account.id)}`, { method: 'DELETE' })
  if (response.ok) {
    if (accounts.value.length === 1 && page.value > 1) page.value--
    await load()
  }
  else error.value = response.error
}

async function importAccount() {
  if (!importForm.credential.trim()) return
  busy.value = 'import'
  const response = await api('/accounts/import', jsonBody('POST', {
    provider: importForm.provider,
    label: importForm.label.trim(),
    credential: parseCredentialInput(importForm.credential),
  }))
  if (response.ok) {
    importing.value = false
    importForm.label = ''
    importForm.credential = ''
    await load()
  } else error.value = response.error
  busy.value = ''
}

onMounted(load)
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">上游账号调度</h2>
        <p class="mt-1 text-xs text-white/40">账号按 route 授权和真实额度桶调度；每次生成都会刷新额度。</p>
      </div>
      <button class="btn-primary" @click="importing = true"><Icon name="plus" class="w-3.5 h-3.5" />导入账号</button>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div class="grid grid-cols-2 md:grid-cols-4 gap-3">
      <div v-for="item in [['匹配账号',stats.total],['本页可用',stats.active],['本页额度受限',stats.cooldown],['本页已禁用',stats.disabled]]" :key="item[0]" class="card p-4">
        <div class="text-[10px] uppercase tracking-wider text-white/40">{{ item[0] }}</div>
        <div class="mt-1 text-2xl font-semibold tabular-nums">{{ item[1] }}</div>
      </div>
    </div>

    <div class="card p-3 flex flex-wrap items-center gap-2">
      <SelectMenu v-model="provider" class="w-36" :options="[{ value: '', label: '全部 Provider' }, ...PROVIDER_OPTIONS]" @update:model-value="search" />
      <SelectMenu v-model="status" class="w-32" :options="[{value:'',label:'全部状态'},{value:'active',label:'可用'},{value:'quota',label:'额度受限'},{value:'pending',label:'待校验'},{value:'disabled',label:'已禁用'}]" @update:model-value="search" />
      <input v-model="query" class="field !py-1.5 text-xs flex-1 min-w-52" placeholder="搜索标签、邮箱或 ID…" @keyup.enter="search" />
      <button class="btn-soft" @click="search"><Icon name="refresh" class="w-3.5 h-3.5" />查询</button>
    </div>

    <div class="space-y-2">
      <article v-for="account in filtered" :key="account.id" class="card overflow-hidden">
        <div class="px-4 py-3.5 grid grid-cols-[auto_minmax(0,1fr)_auto] lg:grid-cols-[auto_minmax(13rem,1fr)_9rem_9rem_8rem_auto] items-center gap-3">
          <button class="text-white/35 hover:text-white/80" @click="toggleOpen(account.id)"><span class="inline-block transition-transform" :class="expanded.has(account.id) && 'rotate-90'">›</span></button>
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span class="provider">{{ account.provider }}</span>
              <strong class="text-xs text-white/85 truncate">{{ account.label || account.email || account.id }}</strong>
            </div>
            <div class="mt-1 text-[10px] text-white/35 truncate">{{ account.email || account.id }} · {{ (account.routes || []).filter((r) => r.enabled !== false).length }} 个 route</div>
          </div>
          <div class="hidden lg:block text-[10px] text-white/40">并发 <span class="text-white/75">{{ account.active_jobs || 0 }} / {{ account.max_concurrency || '∞' }}</span></div>
          <div class="hidden lg:block text-[10px] text-white/40">权重 <span class="text-white/75">{{ account.weight ?? 1 }}</span></div>
          <div class="hidden lg:block"><span class="status" :class="`status-${account.status}`">{{ account.status || 'unknown' }}</span></div>
          <div class="flex items-center gap-1 justify-end">
            <button class="icon-btn" title="校验账号并刷新真实额度" :disabled="busy === `${account.id}:quota`" @click="refreshQuota(account)"><Icon name="refresh" class="w-3.5 h-3.5" /></button>
            <button class="switch" :class="!['disabled','auth_error'].includes(account.status) && 'on'" @click="toggleAccount(account)"><span></span></button>
            <button class="icon-btn danger" title="删除" @click="removeAccount(account)"><Icon name="trash" class="w-3.5 h-3.5" /></button>
          </div>
        </div>

        <div v-if="expanded.has(account.id)" class="border-t border-white/[0.06] p-4 md:px-11 bg-white/[0.015] space-y-5">
          <div>
            <div class="section-title">额度桶</div>
            <div v-if="(account.quota_buckets || []).length" class="grid md:grid-cols-2 xl:grid-cols-3 gap-2 mt-2">
              <div v-for="bucket in account.quota_buckets" :key="bucket.id || bucket.name" class="quota">
                <div class="flex justify-between gap-3 text-[10px]"><span class="text-white/65">{{ bucket.name || bucket.model_id || '默认额度' }}</span><span class="text-white/40">{{ quotaValue(bucket.remaining, bucket.unit) }} / {{ quotaValue(bucket.total, bucket.unit) }}</span></div>
                <div class="mt-2 h-1.5 rounded-full bg-white/[0.07] overflow-hidden"><span class="block h-full bg-gradient-to-r from-violet-500 to-cyan-400" :style="{width:`${quotaPercent(bucket)}%`}"></span></div>
                <div class="mt-2 flex justify-between text-[9px] text-white/30"><span>已预占 {{ quotaValue(bucket.reserved || 0, bucket.unit) }}</span><span>重置 {{ fmtTime(bucket.reset_at) }}</span></div>
              </div>
            </div>
            <p v-else class="mt-2 text-xs text-white/30">暂无额度桶，请刷新账号授权。</p>
          </div>
          <div>
            <div class="section-title">Route 授权</div>
            <div class="mt-2 flex flex-wrap gap-1.5">
              <span v-for="route in account.routes || []" :key="route.id" class="route" :class="route.enabled === false && 'off'">{{ route.model_id || route.logical_model_id }} <small>{{ route.enabled === false ? '停用' : '可用' }}</small></span>
              <span v-if="!(account.routes || []).length" class="text-xs text-white/30">暂无 route 授权</span>
            </div>
          </div>
          <div class="grid md:grid-cols-2 gap-3 max-w-2xl">
            <label><span class="section-title">权重</span><input type="number" min="0" class="field mt-1.5" :value="account.weight ?? 1" @change="patchAccount(account,{weight:Number($event.target.value)},'weight')" /></label>
            <label><span class="section-title">账号并发（0 = 不限）</span><input type="number" min="0" class="field mt-1.5" :value="account.max_concurrency || 0" @change="patchAccount(account,{max_concurrency:Number($event.target.value)},'concurrency')" /></label>
          </div>
        </div>
      </article>
      <div v-if="!loading && !filtered.length" class="card py-16 text-center text-xs text-white/35">没有匹配的账号</div>
    </div>

    <div class="flex items-center justify-between text-xs text-white/35">
      <span>共 {{ total }} 个账号，本页 {{ accounts.length }} 个</span>
      <div class="flex items-center gap-2"><button class="btn-soft" :disabled="page <= 1" @click="go(-1)">上一页</button><span>{{ page }} / {{ pages }}</span><button class="btn-soft" :disabled="page >= pages" @click="go(1)">下一页</button></div>
    </div>

    <div v-if="importing" class="modal-bg" @click.self="importing = false">
      <form class="modal-card" @submit.prevent="importAccount">
        <div class="flex items-center justify-between"><h3 class="font-semibold text-white/90">导入上游账号</h3><button type="button" @click="importing = false"><Icon name="close" class="w-4 h-4" /></button></div>
        <label class="block"><span class="section-title">Provider</span><SelectMenu v-model="importForm.provider" class="mt-1.5" :options="PROVIDER_OPTIONS.filter(option => option.value !== 'custom')" /></label>
        <label class="block"><span class="section-title">显示标签（可选）</span><input v-model="importForm.label" class="field mt-1.5" placeholder="例如：BytePlus A" /></label>
        <label class="block"><span class="section-title">凭据</span><textarea v-model="importForm.credential" rows="8" class="field mt-1.5 resize-y font-mono text-xs" :placeholder="importForm.provider === 'byteplus' ? 'Cookie Header、cookie_string 或浏览器 cookies[] JSON' : '粘贴 Token、Cookie 或账号 JSON'" /></label>
        <p class="text-[10px] leading-5 text-white/35">导入后立即校验身份、route 授权和额度。前端不保存也不回显凭据。</p>
        <button class="btn-primary w-full justify-center" :disabled="busy === 'import'">{{ busy === 'import' ? '校验中…' : '导入并校验' }}</button>
      </form>
    </div>

  </section>
</template>

<style scoped>
.notice { border-radius: .7rem; padding: .7rem .9rem; font-size: .72rem; color: rgb(253 164 175); background: rgb(244 63 94 / .09); box-shadow: inset 0 0 0 1px rgb(244 63 94 / .22); }
.provider,.status { display: inline-flex; border-radius: 999px; padding: .15rem .48rem; font: 600 .6rem ui-monospace,SFMono-Regular,monospace; color: rgb(196 181 253); background: rgb(139 92 246 / .12); box-shadow: inset 0 0 0 1px rgb(167 139 250 / .2); }
.status { color: rgb(255 255 255 / .5); background: rgb(255 255 255 / .05); }
.status-active,.status-healthy,.status-enabled { color: rgb(110 231 183); background: rgb(16 185 129 / .1); }
.status-cooldown { color: rgb(252 211 77); background: rgb(245 158 11 / .1); }
.status-disabled,.status-auth_error { color: rgb(253 164 175); background: rgb(244 63 94 / .1); }
.icon-btn { width: 1.9rem; height: 1.9rem; display: inline-grid; place-items: center; flex: none; border-radius: .5rem; color: rgb(255 255 255 / .55); background: rgb(255 255 255 / .04); box-shadow: inset 0 0 0 1px rgb(255 255 255 / .07); }
.icon-btn:hover { color: white; background: rgb(255 255 255 / .09); }.icon-btn.danger { color: rgb(253 164 175); }
.switch { position: relative; width: 2.2rem; height: 1.25rem; flex: none; border-radius: 999px; background: rgb(255 255 255 / .12); }.switch span { position:absolute;width:.95rem;height:.95rem;left:.15rem;top:.15rem;border-radius:999px;background:white;transition:transform .15s}.switch.on{background:rgb(16 185 129 / .7)}.switch.on span{transform:translateX(.95rem)}
.section-title { font-size: .65rem; font-weight: 600; letter-spacing: .04em; color: rgb(255 255 255 / .42); }
.quota { border-radius: .65rem; padding: .75rem; background: rgb(255 255 255 / .03); box-shadow: inset 0 0 0 1px rgb(255 255 255 / .055); }
.route { display:inline-flex;align-items:center;gap:.4rem;border-radius:999px;padding:.25rem .55rem;font:.62rem ui-monospace,SFMono-Regular,monospace;color:rgb(167 243 208);background:rgb(16 185 129 / .09);box-shadow:inset 0 0 0 1px rgb(16 185 129 / .18)}.route small{opacity:.55}.route.off{color:rgb(255 255 255 / .35);background:rgb(255 255 255 / .035)}
.modal-bg { position:fixed;inset:0;z-index:50;display:grid;place-items:center;padding:1rem;background:rgb(0 0 0 / .65);backdrop-filter:blur(6px) }.modal-card{width:100%;max-width:36rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .7);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .45),inset 0 0 0 1px rgb(255 255 255 / .08)}
</style>
