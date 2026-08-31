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
const providerCounts = ref({})
const page = ref(1)
const limit = 20
const query = ref('')
const provider = ref('')
const status = ref('')
const busy = ref('')
const importing = ref(false)
const importForm = reactive({ provider: 'byteplus', label: '', credential: '' })

const pages = computed(() => Math.max(1, Math.ceil(total.value / limit)))
const pageItems = computed(() => {
  const last = pages.value
  if (last <= 7) return Array.from({ length: last }, (_, index) => index + 1)
  const wanted = new Set([1, last, page.value - 1, page.value, page.value + 1])
  if (page.value <= 4) [2, 3, 4, 5].forEach((value) => wanted.add(value))
  if (page.value >= last - 3) [last - 4, last - 3, last - 2, last - 1].forEach((value) => wanted.add(value))
  const ordered = [...wanted].filter((value) => value >= 1 && value <= last).sort((a, b) => a - b)
  return ordered.flatMap((value, index) => index > 0 && value - ordered[index - 1] > 1 ? [`gap-${value}`, value] : [value])
})

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
    providerCounts.value = response.data?.provider_counts || {}
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

function search() {
  page.value = 1
  load()
}

function selectProvider(value) {
  provider.value = provider.value === value ? '' : value
  search()
}

function goTo(target) {
  const next = Math.max(1, Math.min(pages.value, Number(target) || 1))
  if (next === page.value) return
  page.value = next
  load()
}

function enabledRoutes(account) {
  return (account.routes || []).filter((route) => route.enabled !== false)
}

function routeTitle(account) {
  return enabledRoutes(account).map((route) => route.model_id || route.logical_model_id).join('、')
}

function firstQuota(account) {
  return (account.quota_buckets || [])[0] || null
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

function statusLabel(value) {
  return ({ active: '可用', quota: '额度受限', pending: '待校验', disabled: '已禁用' })[value] || value || '未知'
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

    <div class="provider-stock" aria-label="各平台账号数量">
      <button v-for="option in PROVIDER_OPTIONS" :key="option.value" :class="provider === option.value && 'on'" @click="selectProvider(option.value)">
        <span>{{ option.label }}</span><strong>{{ Number(providerCounts[option.value] || 0).toLocaleString('zh-CN') }}</strong>
      </button>
    </div>

    <div class="card p-3 flex flex-wrap items-center gap-2">
      <SelectMenu v-model="provider" class="w-36" :options="[{ value: '', label: '全部 Provider' }, ...PROVIDER_OPTIONS]" @update:model-value="search" />
      <SelectMenu v-model="status" class="w-32" :options="[{value:'',label:'全部状态'},{value:'active',label:'可用'},{value:'quota',label:'额度受限'},{value:'pending',label:'待校验'},{value:'disabled',label:'已禁用'}]" @update:model-value="search" />
      <input v-model="query" class="field !py-1.5 text-xs flex-1 min-w-52" placeholder="搜索标签、邮箱或 ID…" @keyup.enter="search" />
      <button class="btn-soft" @click="search"><Icon name="refresh" class="w-3.5 h-3.5" />查询</button>
    </div>

    <div class="card overflow-hidden">
      <div class="px-4 py-3 flex flex-wrap items-center justify-between gap-3 border-b border-white/[0.06]">
        <div class="text-xs text-white/45">匹配 <strong class="text-sm text-white/85 tabular-nums">{{ total }}</strong> 个账号 <span class="ml-2">每页 {{ limit }} 个</span></div>
        <nav class="pagination" aria-label="账号分页">
          <button :disabled="page <= 1" @click="goTo(page - 1)">上一页</button>
          <template v-for="item in pageItems" :key="item">
            <span v-if="typeof item !== 'number'">…</span>
            <button v-else :class="item === page && 'on'" :aria-current="item === page ? 'page' : undefined" @click="goTo(item)">{{ item }}</button>
          </template>
          <button :disabled="page >= pages" @click="goTo(page + 1)">下一页</button>
        </nav>
      </div>

      <div class="overflow-x-auto">
        <table class="account-table">
          <thead><tr><th>账号</th><th>Provider</th><th>Route 授权</th><th>额度</th><th>并发</th><th>权重</th><th>状态</th><th class="text-right">操作</th></tr></thead>
          <tbody>
            <tr v-if="loading"><td colspan="8" class="empty-cell">正在加载账号…</td></tr>
            <tr v-else-if="!accounts.length"><td colspan="8" class="empty-cell">没有匹配的账号</td></tr>
            <tr v-for="account in accounts" v-else :key="account.id">
              <td class="account-cell">
                <strong :title="account.label || account.email || account.id">{{ account.label || account.email || account.id }}</strong>
                <span :title="account.email || account.id">{{ account.email || account.id }}</span>
              </td>
              <td><span class="provider">{{ account.provider }}</span></td>
              <td class="routes-cell" :title="routeTitle(account)">
                <div><b>{{ enabledRoutes(account).length }}</b> / {{ (account.routes || []).length }} 个可用</div>
                <div class="route-preview">
                  <span v-for="route in enabledRoutes(account).slice(0, 2)" :key="route.id">{{ route.model_id || route.logical_model_id }}</span>
                  <span v-if="enabledRoutes(account).length > 2">+{{ enabledRoutes(account).length - 2 }}</span>
                </div>
              </td>
              <td class="quota-cell">
                <template v-if="firstQuota(account)">
                  <div class="quota-line"><span :title="firstQuota(account).name">{{ firstQuota(account).name }}</span><b>{{ quotaValue(firstQuota(account).remaining, firstQuota(account).unit) }}</b></div>
                  <div class="quota-bar"><span :style="{ width: `${quotaPercent(firstQuota(account))}%` }"></span></div>
                  <small>共 {{ quotaValue(firstQuota(account).total, firstQuota(account).unit) }}<template v-if="(account.quota_buckets || []).length > 1"> · {{ account.quota_buckets.length }} 个额度桶</template></small>
                </template>
                <span v-else class="text-white/30">未获取</span>
              </td>
              <td>
                <div class="job-count">运行中 {{ account.active_jobs || 0 }}</div>
                <input type="number" min="0" class="compact-input" :value="account.max_concurrency || 0" title="账号并发，0 表示不限" @change="patchAccount(account,{max_concurrency:Number($event.target.value)},'concurrency')" />
              </td>
              <td><input type="number" min="0" class="compact-input" :value="account.weight ?? 1" title="调度权重" @change="patchAccount(account,{weight:Number($event.target.value)},'weight')" /></td>
              <td><span class="status" :class="`status-${account.status}`">{{ statusLabel(account.status) }}</span></td>
              <td><div class="flex items-center gap-1 justify-end">
                <button class="icon-btn" title="校验账号并刷新真实额度" :disabled="busy === `${account.id}:quota`" @click="refreshQuota(account)"><Icon name="refresh" class="w-3.5 h-3.5" /></button>
                <button class="switch" :class="!['disabled','auth_error'].includes(account.status) && 'on'" :title="account.status === 'disabled' ? '启用账号' : '停用账号'" @click="toggleAccount(account)"><span></span></button>
                <button class="icon-btn danger" title="删除账号" @click="removeAccount(account)"><Icon name="trash" class="w-3.5 h-3.5" /></button>
              </div></td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="total > limit" class="px-4 py-3 flex items-center justify-between gap-3 border-t border-white/[0.06] text-xs text-white/40">
        <span>第 {{ page }} / {{ pages }} 页</span>
        <nav class="pagination" aria-label="账号底部分页"><button :disabled="page <= 1" @click="goTo(page - 1)">上一页</button><button :disabled="page >= pages" @click="goTo(page + 1)">下一页</button></nav>
      </div>
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
.provider-stock { display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:.5rem }
.provider-stock button { display:flex;align-items:center;justify-content:space-between;gap:.75rem;min-width:0;border:1px solid var(--hairline);border-radius:.7rem;padding:.65rem .8rem;color:var(--fg-3);background:var(--surface);transition:border-color .15s,background-color .15s,color .15s }
.provider-stock button:hover { color:var(--fg);background:var(--hover) }.provider-stock button.on{color:rgb(109 40 217);border-color:rgb(139 92 246 / .45);background:rgb(139 92 246 / .09)}
.provider-stock span { overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:.65rem;font-weight:600 }.provider-stock strong{color:var(--fg);font-size:1rem;font-variant-numeric:tabular-nums}
@media (min-width: 768px) { .provider-stock{grid-template-columns:repeat(4,minmax(0,1fr))} }
@media (min-width: 1280px) { .provider-stock{grid-template-columns:repeat(7,minmax(0,1fr))} }
.provider,.status { display: inline-flex; border-radius: 999px; padding: .15rem .48rem; font: 600 .6rem ui-monospace,SFMono-Regular,monospace; color: rgb(196 181 253); background: rgb(139 92 246 / .12); box-shadow: inset 0 0 0 1px rgb(167 139 250 / .2); }
.status { color: rgb(255 255 255 / .5); background: rgb(255 255 255 / .05); }
.status-active,.status-healthy,.status-enabled { color: rgb(110 231 183); background: rgb(16 185 129 / .1); }
.status-cooldown,.status-quota,.status-pending { color: rgb(252 211 77); background: rgb(245 158 11 / .1); }
.status-disabled,.status-auth_error { color: rgb(253 164 175); background: rgb(244 63 94 / .1); }
.icon-btn { width: 1.9rem; height: 1.9rem; display: inline-grid; place-items: center; flex: none; border-radius: .5rem; color: var(--fg-3); background: var(--surface-2); box-shadow: inset 0 0 0 1px var(--hairline); }
.icon-btn:hover { color: var(--fg); background: var(--hover); }.icon-btn.danger { color: rgb(244 63 94 / .75); }
.switch { position: relative; width: 2.2rem; height: 1.25rem; flex: none; border-radius: 999px; background: rgb(255 255 255 / .12); }.switch span { position:absolute;width:.95rem;height:.95rem;left:.15rem;top:.15rem;border-radius:999px;background:white;transition:transform .15s}.switch.on{background:rgb(16 185 129 / .7)}.switch.on span{transform:translateX(.95rem)}
.section-title { font-size: .65rem; font-weight: 600; letter-spacing: .04em; color: var(--fg-3); }
.account-table { width: 100%; min-width: 1240px; table-layout: fixed; border-collapse: collapse; font-size: .72rem; }
.account-table th { padding: .65rem .75rem; color: var(--fg-3); background: var(--surface-2); font-size: .62rem; font-weight: 600; letter-spacing: .04em; text-align: left; }
.account-table td { padding: .7rem .75rem; color: var(--fg-2); vertical-align: middle; border-top: 1px solid var(--hairline); }
.account-table tbody tr { transition: background-color .15s ease; }
.account-table tbody tr:hover { background: var(--hover); }
.account-table th:nth-child(1) { width: 20%; }.account-table th:nth-child(2) { width: 8%; }.account-table th:nth-child(3) { width: 20%; }.account-table th:nth-child(4) { width: 18%; }.account-table th:nth-child(5) { width: 9%; }.account-table th:nth-child(6) { width: 7%; }.account-table th:nth-child(7) { width: 8%; }.account-table th:nth-child(8) { width: 10%; }
.account-cell strong,.account-cell span { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.account-cell strong { color: var(--fg); font-size: .74rem; font-weight: 600; }.account-cell span { margin-top: .25rem; color: var(--fg-3); font-size: .62rem; }
.routes-cell > div:first-child { color: var(--fg-3); font-size: .62rem; }.routes-cell b { color: var(--fg); font-size: .72rem; }
.route-preview { display: flex; align-items: center; gap: .25rem; min-width: 0; margin-top: .35rem; overflow: hidden; }
.route-preview span { flex: none; max-width: 7.5rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; border-radius: 999px; padding: .13rem .38rem; color: rgb(5 150 105); background: rgb(16 185 129 / .1); font: 500 .55rem ui-monospace,SFMono-Regular,monospace; }
.quota-line { display:flex;align-items:center;justify-content:space-between;gap:.5rem;font-size:.58rem;color:var(--fg-3) }.quota-line span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.quota-line b{flex:none;color:var(--fg-2);font-weight:600}
.quota-bar { height:.25rem;margin-top:.4rem;border-radius:999px;overflow:hidden;background:var(--surface-2) }.quota-bar span{display:block;height:100%;border-radius:inherit;background:linear-gradient(90deg,rgb(139 92 246),rgb(34 211 238))}
.quota-cell small { display:block;margin-top:.3rem;color:var(--fg-faint);font-size:.56rem }
.job-count { margin-bottom:.3rem;color:var(--fg-3);font-size:.58rem }
.compact-input { width:4.25rem;border:1px solid var(--hairline);border-radius:.45rem;padding:.3rem .45rem;color:var(--fg);background:var(--surface);font-size:.68rem;outline:none }
.compact-input:focus { border-color:rgb(139 92 246 / .65);box-shadow:0 0 0 2px rgb(139 92 246 / .12) }
.empty-cell { height:10rem;text-align:center;color:var(--fg-3)!important }
.pagination { display:flex;align-items:center;gap:.25rem }.pagination button{min-width:1.8rem;height:1.8rem;padding:0 .5rem;border-radius:.45rem;color:var(--fg-2);background:var(--surface-2);box-shadow:inset 0 0 0 1px var(--hairline);font-size:.65rem}.pagination button:hover:not(:disabled){color:var(--fg);background:var(--hover)}.pagination button.on{color:white;background:rgb(124 58 237);box-shadow:none}.pagination button:disabled{opacity:.35}.pagination span{padding:0 .2rem;color:var(--fg-faint)}
.modal-bg { position:fixed;inset:0;z-index:50;display:grid;place-items:center;padding:1rem;background:rgb(0 0 0 / .65);backdrop-filter:blur(6px) }.modal-card{width:100%;max-width:36rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .7);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .45),inset 0 0 0 1px rgb(255 255 255 / .08)}
</style>
