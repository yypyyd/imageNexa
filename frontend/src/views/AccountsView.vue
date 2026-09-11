<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import SelectMenu from '../components/SelectMenu.vue'
import AccountTestModal from '../components/AccountTestModal.vue'
import { api, jsonBody, listOf } from '../api'
import { ACCOUNT_PROVIDERS } from '../models'
import { parseCredentialFile, parseCredentialImports, uniqueCredentialImports } from '../credential'

const ALLOWED_PROVIDERS = ACCOUNT_PROVIDERS
const PROVIDER_OPTIONS = ALLOWED_PROVIDERS.map((value) => ({ value, label: value === 'chatgpt' ? 'ChatGPT' : value === 'dola' ? 'Dola' : value[0].toUpperCase() + value.slice(1) }))
const PROVIDER_LABELS = Object.fromEntries(PROVIDER_OPTIONS.map((option) => [option.value, option.label]))

const accounts = ref([])
const loading = ref(true)
const error = ref('')
const total = ref(0)
const providerCounts = ref({})
const providerHealth = ref({})
const page = ref(1)
const limit = 20
const query = ref('')
const provider = ref('')
const status = ref('')
const busy = ref('')
const importing = ref(false)
const testingAccount = ref(null)
const allModels = ref([])
const importForm = reactive({ credential: '', weight: 0 })
const fileInput = ref(null)
const fileItems = ref([])
const fileNames = ref([])
const importStatus = ref('')
const importStatusError = ref(false)
const selected = ref(new Set())
const expanded = ref(new Set())

const importItems = computed(() => uniqueCredentialImports([
  ...parseCredentialImports(importForm.credential),
  ...fileItems.value,
]))
const detectedProviders = computed(() => {
  const counts = {}
  for (const item of importItems.value) counts[item.provider] = (counts[item.provider] || 0) + 1
  return counts
})

const pages = computed(() => Math.max(1, Math.ceil(total.value / limit)))
const allSelected = computed(() => accounts.value.length > 0 && accounts.value.every((account) => selected.value.has(account.id)))
const deadCount = computed(() => Object.values(providerHealth.value).reduce((sum, counts) => sum + Number(counts?.dead || 0), 0))
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

async function load(quiet = false) {
  if (quiet && loading.value) return
  if (!quiet) loading.value = true
  const response = await api(listURL())
  if (response.ok) {
    accounts.value = listOf(response.data)
    total.value = Number(response.data?.total ?? accounts.value.length)
    providerCounts.value = response.data?.provider_counts || {}
    providerHealth.value = response.data?.provider_health || {}
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

async function loadModels() {
  const response = await api('/logical-models')
  if (response.ok) allModels.value = listOf(response.data)
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

function toggleSelected(id) {
  const next = new Set(selected.value)
  next.has(id) ? next.delete(id) : next.add(id)
  selected.value = next
}

function toggleSelectAll() {
  const next = new Set(selected.value)
  if (allSelected.value) accounts.value.forEach((account) => next.delete(account.id))
  else accounts.value.forEach((account) => next.add(account.id))
  selected.value = next
}

function toggleExpanded(id) {
  const next = new Set(expanded.value)
  next.has(id) ? next.delete(id) : next.add(id)
  expanded.value = next
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

function fmtTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function statusLabel(value) {
  return ({ active: '可用', quota: '额度受限', pending: '待校验', disabled: '已禁用', auth_error: '凭据失效', cooldown: '冷却中' })[value] || value || '未知'
}

function dolaReadinessLabel(account) {
 return ({pending:'待验证',checking:'正在验证协议会话',ready:'协议会话已就绪',login_required:'需要更新登录 Cookie',challenge:'需要人机验证',retry:'等待自动重试',failed:'验证失败，请重新验证'})[account.readiness] || '待验证'
}

function sessionLabel(account) {
  return ({ valid: '会话', expiring: '会话将到期', expired: '会话已过期', unknown: '会话到期未知' })[account.session_state] || '会话'
}

async function patchAccount(account, patch, key) {
  busy.value = `${account.id}:${key}`
  const response = await api(`/accounts/${encodeURIComponent(account.id)}`, jsonBody('PATCH', patch))
  if (!response.ok) error.value = response.error
  else Object.assign(account, response.data?.data || response.data || patch)
  busy.value = ''
}

async function toggleAccount(account) {
  if (account.provider !== 'dola' && !['active', 'disabled'].includes(account.status)) return
  await patchAccount(account, { status: account.status === 'disabled' ? 'active' : 'disabled' }, 'status')
}

async function toggleAccountRoute(account, route) {
  const old = route.enabled !== false
  route.enabled = !old
  busy.value = `${account.id}:route:${route.id}`
  const response = await api(`/accounts/${encodeURIComponent(account.id)}/routes/${encodeURIComponent(route.id)}`, jsonBody('PATCH', { enabled: !old }))
  if (!response.ok) {
    route.enabled = old
    error.value = response.error
  }
  busy.value = ''
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
    const next = new Set(selected.value)
    next.delete(account.id)
    selected.value = next
    if (accounts.value.length === 1 && page.value > 1) page.value--
    await load()
  }
  else error.value = response.error
}

async function removeSelected() {
  const ids = [...selected.value]
  if (!ids.length || !confirm(`确认删除选中的 ${ids.length} 个账号？已产生的日志仍会保留快照。`)) return
  busy.value = 'delete-selected'
  const failures = []
  for (const id of ids) {
    const response = await api(`/accounts/${encodeURIComponent(id)}`, { method: 'DELETE' })
    if (!response.ok) failures.push(id)
  }
  selected.value = new Set(failures)
  if (failures.length) error.value = `${failures.length} 个账号删除失败，请重试`
  if (page.value > 1 && accounts.value.every((account) => ids.includes(account.id))) page.value--
  await load()
  busy.value = ''
}

async function removeDeadAccounts() {
  if (!deadCount.value || !confirm(`确认删除全部 ${deadCount.value} 个凭据失效账号？手动停用但凭据有效的账号不会删除。`)) return
  busy.value = 'delete-dead'
  const response = await api('/accounts/delete-dead', jsonBody('POST', {}))
  if (!response.ok) error.value = response.error
  else {
    selected.value = new Set()
    page.value = 1
    await load()
  }
  busy.value = ''
}

async function importAccount() {
  const items = importItems.value
  if (!items.length) {
    error.value = '未识别到可导入的账号凭据'
    return
  }
  busy.value = 'import'
  let succeeded = 0
  const failures = []
  for (const item of items) {
    const response = await api('/accounts/import', jsonBody('POST', {
      provider: item.provider,
      credential: item.credential,
    }))
    if (response.ok) {
      succeeded++
      const accountID = response.data?.data?.id
      const weight = Number(importForm.weight) || 0
      if (accountID && weight !== 0) {
        await api(`/accounts/${encodeURIComponent(accountID)}`, jsonBody('PATCH', { weight }))
      }
    } else failures.push(`${item.provider}: ${response.error}`)
  }
  if (!failures.length) {
    closeImportModal()
    await load()
  } else {
    importStatus.value = `成功 ${succeeded} · 失败 ${failures.length} · ${failures.slice(0, 3).join('；')}`
    importStatusError.value = true
    if (succeeded) await load()
  }
  busy.value = ''
}

async function selectImportFiles(event) {
  const files = [...(event.target.files || [])]
  event.target.value = ''
  if (!files.length) return
  try {
    const parsed = []
    for (const file of files) parsed.push(...await parseCredentialFile(file))
    fileItems.value = uniqueCredentialImports(parsed)
    fileNames.value = files.map((file) => file.name)
    importStatus.value = `已读取 ${files.length} 个文件，识别到 ${fileItems.value.length} 个账号`
    importStatusError.value = false
  } catch (fileError) {
    fileItems.value = []
    fileNames.value = []
    importStatus.value = fileError?.message || String(fileError)
    importStatusError.value = true
  }
}

function clearImportFiles() {
  fileItems.value = []
  fileNames.value = []
  importStatus.value = ''
  importStatusError.value = false
}

function closeImportModal() {
  importing.value = false
  importForm.credential = ''
  importForm.weight = 0
  clearImportFiles()
}

let readinessTimer
onMounted(() => {
  load(); loadModels()
  readinessTimer = setInterval(() => {
    if (accounts.value.some(a => a.provider === 'dola' && a.status !== 'disabled')) load(true)
  }, 5000)
})
onUnmounted(() => clearInterval(readinessTimer))
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">上游账号调度</h2>
        <p class="mt-1 text-xs text-white/40">账号按 route 授权和真实额度桶调度；每次生成都会刷新额度。</p>
      </div>
      <div class="flex items-center gap-2">
        <button v-if="selected.size" class="btn-soft danger" :disabled="busy === 'delete-selected'" @click="removeSelected"><Icon name="trash" class="w-3.5 h-3.5" />删除选中 ({{ selected.size }})</button>
        <button v-if="deadCount" class="btn-soft danger" :disabled="busy === 'delete-dead'" @click="removeDeadAccounts"><Icon name="trash" class="w-3.5 h-3.5" />删除异常账号 ({{ deadCount }})</button>
        <button class="btn-primary" @click="importing = true"><Icon name="plus" class="w-3.5 h-3.5" />导入账号</button>
      </div>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div class="provider-stock" aria-label="各平台账号数量">
      <button v-for="option in PROVIDER_OPTIONS" :key="option.value" :class="provider === option.value && 'on'" @click="selectProvider(option.value)">
        <span>{{ option.label }}</span><strong>{{ Number(providerCounts[option.value] || 0).toLocaleString('zh-CN') }}</strong><small title="可用 / 失效 / 限额"><i class="ok">{{ providerHealth[option.value]?.active || 0 }}</i> / <i class="bad">{{ providerHealth[option.value]?.dead || 0 }}</i> / <i class="warn">{{ providerHealth[option.value]?.quota || 0 }}</i></small>
      </button>
    </div>

    <div class="card p-3 flex flex-wrap items-center gap-2">
      <SelectMenu v-model="provider" class="w-36" :options="[{ value: '', label: '全部 Provider' }, ...PROVIDER_OPTIONS]" @update:model-value="search" />
      <SelectMenu v-model="status" class="w-32" :options="[{value:'',label:'全部状态'},{value:'active',label:'可用'},{value:'quota',label:'额度受限'},{value:'pending',label:'待校验'},{value:'disabled',label:'已禁用'},{value:'auth_error',label:'凭据失效'},{value:'cooldown',label:'冷却中'}]" @update:model-value="search" />
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
          <thead><tr><th class="check-cell"><input type="checkbox" class="chk" :checked="allSelected" title="全选当前页" @change="toggleSelectAll" /></th><th>账号</th><th>Provider</th><th>Route 授权</th><th>额度</th><th>运行统计</th><th>时间</th><th>并发</th><th>权重</th><th>状态</th><th class="text-right">操作</th></tr></thead>
          <tbody>
            <tr v-if="loading"><td colspan="11" class="empty-cell">正在加载账号…</td></tr>
            <tr v-else-if="!accounts.length"><td colspan="11" class="empty-cell">没有匹配的账号</td></tr>
            <template v-for="account in accounts" v-else :key="account.id">
            <tr>
              <td class="check-cell"><input type="checkbox" class="chk" :checked="selected.has(account.id)" :aria-label="`选择 ${account.label || account.id}`" @change="toggleSelected(account.id)" /></td>
              <td class="account-cell">
                <strong :title="account.label || account.email || account.id">{{ account.label || account.email || account.id }}</strong>
                <span :title="account.email || account.id">{{ account.email || account.id }}</span>
              </td>
              <td><span class="provider">{{ account.provider }}</span></td>
              <td class="routes-cell" :title="routeTitle(account)">
                <button class="route-summary" title="展开并编辑该账号的 Route 授权" @click="toggleExpanded(account.id)"><span :class="expanded.has(account.id) && 'rotate-90'">›</span><b>{{ enabledRoutes(account).length }}</b> / {{ (account.routes || []).length }} 个可用</button>
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
              <td class="stats-cell"><div><b>{{ account.active_jobs || 0 }}</b> 在途</div><small><span class="ok">{{ account.success_total || 0 }} 成功</span> · <span :class="Number(account.fail_total || 0) > 0 && 'bad'">{{ account.fail_total || 0 }} 失败</span></small></td>
              <td class="time-cell"><div title="最后使用">用 {{ fmtTime(account.last_used_at) }}</div><small title="导入/创建时间">建 {{ fmtTime(account.created_at) }}</small><small v-if="account.provider === 'byteplus'" :class="{ warn: account.session_state === 'expiring', bad: account.session_state === 'expired' }" :title="sessionLabel(account)">{{ sessionLabel(account) }} {{ fmtTime(account.session_expires_at) }}</small></td>
              <td>
                <div class="job-count">运行中 {{ account.active_jobs || 0 }}</div>
                <input type="number" min="0" max="1000" class="compact-input" :value="account.max_concurrency || 0" title="账号并发，0 表示使用 Provider 默认值" @change="patchAccount(account,{max_concurrency:Number($event.target.value)},'concurrency')" />
              </td>
              <td><input type="number" min="-1000" max="1000" class="compact-input" :value="account.weight ?? 0" title="调度权重，高的优先" @change="patchAccount(account,{weight:Number($event.target.value)},'weight')" /></td>
              <td><span class="status" :class="`status-${account.status}`">{{ statusLabel(account.status) }}</span><div v-if="account.provider === 'dola'" class="text-xs mt-1" :class="account.readiness === 'ready' ? 'ok' : 'warn'" :title="account.readiness_detail">{{ dolaReadinessLabel(account) }}</div><small v-if="account.provider === 'dola' && account.readiness_detail && account.readiness !== 'ready'" class="block max-w-48 text-white/50">{{ account.readiness_detail }}</small><div v-if="account.image_limited || account.video_limited" class="limit-flags"><span v-if="account.image_limited">图片限额</span><span v-if="account.video_limited">视频限额</span></div></td>
              <td><div class="flex items-center gap-1 justify-end">
                <button class="icon-btn test-action" title="账号能力测试" @click="testingAccount = account"><Icon name="test" class="w-3.5 h-3.5" /></button>
                <button class="icon-btn" :title="account.provider === 'dola' ? '重新验证协议会话（不生成视频）' : '校验账号并刷新真实额度'" :disabled="busy === `${account.id}:quota`" @click="refreshQuota(account)"><Icon name="refresh" class="w-3.5 h-3.5" /></button>
                <button class="switch" :class="(account.provider === 'dola' ? account.status !== 'disabled' : account.status === 'active') && 'on'" :disabled="account.provider !== 'dola' && !['active','disabled'].includes(account.status)" :title="account.status === 'disabled' ? '启用账号' : (account.provider === 'dola' || account.status === 'active' ? '停用账号' : `${statusLabel(account.status)}状态不可手动切换`)" @click="toggleAccount(account)"><span></span></button>
                <button class="icon-btn danger" title="删除账号" @click="removeAccount(account)"><Icon name="trash" class="w-3.5 h-3.5" /></button>
              </div></td>
            </tr>
            <tr v-if="expanded.has(account.id)" class="detail-row">
              <td colspan="11">
                <div class="detail-grid">
                  <section><h4>Route 授权</h4><div class="route-list"><label v-for="route in account.routes || []" :key="route.id"><span><code>{{ route.model_id || route.logical_model_id }}</code><small>{{ route.route_id }}</small></span><button class="switch" :class="route.enabled !== false && 'on'" :disabled="busy === `${account.id}:route:${route.id}`" @click="toggleAccountRoute(account, route)"><span></span></button></label><p v-if="!(account.routes || []).length">该账号没有绑定 canonical route</p></div></section>
                  <section><h4>全部额度桶</h4><div class="bucket-list"><div v-for="bucket in account.quota_buckets || []" :key="bucket.name"><code>{{ bucket.name }}</code><b>{{ quotaValue(bucket.remaining, bucket.unit) }} / {{ quotaValue(bucket.total, bucket.unit) }}</b><small>预留 {{ quotaValue(bucket.reserved, bucket.unit) }} · 重置 {{ fmtTime(bucket.reset_at) }} · 刷新 {{ fmtTime(bucket.refreshed_at) }}</small></div><p v-if="!(account.quota_buckets || []).length">尚未获取额度</p></div></section>
                  <section><h4>失败与限制</h4><div class="health-detail"><div><span>累计成功</span><b class="ok">{{ account.success_total || 0 }}</b></div><div><span>累计失败</span><b :class="Number(account.fail_total || 0) > 0 && 'bad'">{{ account.fail_total || 0 }}</b></div><div><span>连续账号失败</span><b :class="Number(account.consecutive_failures || 0) > 0 && 'bad'">{{ account.consecutive_failures || 0 }}</b></div><div><span>上游服务失败</span><b :class="Number(account.upstream_failures || 0) > 0 && 'warn'">{{ account.upstream_failures || 0 }}</b></div></div></section>
                </div>
              </td>
            </tr>
            </template>
          </tbody>
        </table>
      </div>

      <div v-if="total > limit" class="px-4 py-3 flex items-center justify-between gap-3 border-t border-white/[0.06] text-xs text-white/40">
        <span>第 {{ page }} / {{ pages }} 页</span>
        <nav class="pagination" aria-label="账号底部分页"><button :disabled="page <= 1" @click="goTo(page - 1)">上一页</button><button :disabled="page >= pages" @click="goTo(page + 1)">下一页</button></nav>
      </div>
    </div>

    <div v-if="importing" class="modal-bg" @click.self="closeImportModal">
      <form class="modal-card" @submit.prevent="importAccount">
        <div class="flex items-center justify-between"><h3 class="font-semibold text-white/90">导入账号</h3><button type="button" @click="closeImportModal"><Icon name="close" class="w-4 h-4" /></button></div>
        <p class="text-[11px] leading-5 text-white/45">自动识别 CPA / Sub2API JSON、ChatGPT / Grok JWT、Adobe / BytePlus / Dola Cookie，以及多行混合凭据。全粘进来即可，无需任何前缀或平台选择。</p>

        <input ref="fileInput" type="file" accept=".json,.zip,application/json,application/zip" multiple class="hidden" @change="selectImportFiles" />
        <div class="file-picker">
          <button type="button" class="btn-soft shrink-0" @click="fileInput?.click()">选择 CPA / Sub 文件</button>
          <span v-if="fileNames.length" class="text-emerald-300 truncate">{{ fileNames.length === 1 ? fileNames[0] : `${fileNames.length} 个文件` }} · {{ fileItems.length }} 个账号</span>
          <span v-else class="truncate">支持 .json、批量 .zip，可多选</span>
          <button v-if="fileNames.length" type="button" class="ml-auto hover:text-rose-300" @click="clearImportFiles">清除</button>
        </div>

        <textarea v-model="importForm.credential" rows="10" class="field resize-none font-mono text-xs" placeholder="也可直接粘贴 CPA / Sub2API JSON、Cookie 字符串或 JWT，自动识别"></textarea>
        <div v-if="importForm.credential.trim() || fileItems.length" class="detection" :class="!importItems.length && 'detection-error'">
          <template v-if="importItems.length">
            <span>✓ 识别到 <strong>{{ importItems.length }}</strong> 个账号</span>
            <b v-for="(count, name) in detectedProviders" :key="name">{{ PROVIDER_LABELS[name] || name }} · {{ count }}</b>
          </template>
          <span v-else>未识别到任何 Cookie 或 JWT</span>
        </div>
        <label class="flex items-center gap-2"><span class="text-[11px] text-white/45 whitespace-nowrap">权重（本批账号，高的优先）</span><input v-model.number="importForm.weight" type="number" class="field !w-24 !py-1.5" placeholder="0" /></label>
        <button class="btn-primary w-full justify-center" :disabled="busy === 'import' || !importItems.length">{{ busy === 'import' ? '校验中…' : (importItems.length ? `识别并导入 (${importItems.length})` : '识别并导入') }}</button>
        <p v-if="importStatus" class="text-[11px]" :class="importStatusError ? 'text-rose-300' : 'text-emerald-300'">{{ importStatus }}</p>
      </form>
    </div>

    <AccountTestModal v-if="testingAccount" :account="testingAccount" :models="allModels" @close="testingAccount = null" />

  </section>
</template>

<style scoped>
.notice { border-radius: .7rem; padding: .7rem .9rem; font-size: .72rem; color: rgb(253 164 175); background: rgb(244 63 94 / .09); box-shadow: inset 0 0 0 1px rgb(244 63 94 / .22); }
.provider-stock { display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:.5rem }
.provider-stock button { display:flex;flex-wrap:wrap;align-items:center;justify-content:space-between;gap:.2rem .75rem;min-width:0;border:1px solid var(--hairline);border-radius:.7rem;padding:.65rem .8rem;color:var(--fg-3);background:var(--surface);transition:border-color .15s,background-color .15s,color .15s }
.provider-stock button:hover { color:var(--fg);background:var(--hover) }.provider-stock button.on{color:rgb(109 40 217);border-color:rgb(139 92 246 / .45);background:rgb(139 92 246 / .09)}
.provider-stock span { overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:.65rem;font-weight:600 }.provider-stock strong{color:var(--fg);font-size:1rem;font-variant-numeric:tabular-nums}
.provider-stock small{width:100%;text-align:left;color:var(--fg-faint);font-size:.55rem}.provider-stock i{font-style:normal}.provider-stock .ok{color:rgb(16 185 129)}.provider-stock .bad{color:rgb(244 63 94)}.provider-stock .warn{color:rgb(245 158 11)}
@media (min-width: 768px) { .provider-stock{grid-template-columns:repeat(4,minmax(0,1fr))} }
@media (min-width: 1280px) { .provider-stock{grid-template-columns:repeat(7,minmax(0,1fr))} }
.provider,.status { display: inline-flex; border-radius: 999px; padding: .15rem .48rem; font: 600 .6rem ui-monospace,SFMono-Regular,monospace; color: rgb(196 181 253); background: rgb(139 92 246 / .12); box-shadow: inset 0 0 0 1px rgb(167 139 250 / .2); }
.status { color: rgb(255 255 255 / .5); background: rgb(255 255 255 / .05); }
.status-active,.status-healthy,.status-enabled { color: rgb(110 231 183); background: rgb(16 185 129 / .1); }
.status-cooldown,.status-quota,.status-pending { color: rgb(252 211 77); background: rgb(245 158 11 / .1); }
.status-disabled,.status-auth_error { color: rgb(253 164 175); background: rgb(244 63 94 / .1); }
.icon-btn { width: 1.9rem; height: 1.9rem; display: inline-grid; place-items: center; flex: none; border-radius: .5rem; color: var(--fg-3); background: var(--surface-2); box-shadow: inset 0 0 0 1px var(--hairline); }
.icon-btn:hover { color: var(--fg); background: var(--hover); }.icon-btn.danger { color: rgb(244 63 94 / .75); }
.btn-soft.danger { color:rgb(253 164 175);background:rgb(244 63 94 / .08);box-shadow:inset 0 0 0 1px rgb(244 63 94 / .18) }
.icon-btn.test-action { color:rgb(124 58 237);background:rgb(139 92 246 / .1);box-shadow:inset 0 0 0 1px rgb(139 92 246 / .2) }.icon-btn.test-action:hover{color:rgb(109 40 217);background:rgb(139 92 246 / .18)}
.switch { position: relative; width: 2.2rem; height: 1.25rem; flex: none; border-radius: 999px; background: rgb(255 255 255 / .12); }.switch span { position:absolute;width:.95rem;height:.95rem;left:.15rem;top:.15rem;border-radius:999px;background:white;transition:transform .15s}.switch.on{background:rgb(16 185 129 / .7)}.switch.on span{transform:translateX(.95rem)}.switch:disabled{opacity:.4;cursor:not-allowed}
.section-title { font-size: .65rem; font-weight: 600; letter-spacing: .04em; color: var(--fg-3); }
.detection { display:flex;align-items:center;gap:.4rem;flex-wrap:wrap;border-radius:.55rem;padding:.55rem .7rem;color:rgb(110 231 183);background:rgb(16 185 129 / .08);font-size:.63rem;box-shadow:inset 0 0 0 1px rgb(16 185 129 / .16) }
.detection b { border-radius:999px;padding:.15rem .45rem;color:rgb(196 181 253);background:rgb(139 92 246 / .12);font:600 .58rem ui-monospace,SFMono-Regular,monospace }.detection-error{color:rgb(253 164 175);background:rgb(244 63 94 / .08);box-shadow:inset 0 0 0 1px rgb(244 63 94 / .16)}
.file-picker { display:flex;align-items:center;gap:.5rem;border:1px dashed rgb(255 255 255 / .18);border-radius:.65rem;padding:.6rem .7rem;color:rgb(255 255 255 / .58);background:rgb(255 255 255 / .05);font-size:.65rem }
.file-picker .btn-soft { color:rgb(255 255 255 / .9);background:rgb(139 92 246 / .2);box-shadow:inset 0 0 0 1px rgb(167 139 250 / .35) }
.file-picker .btn-soft:hover { color:white;background:rgb(139 92 246 / .32) }
.account-table { width: 100%; min-width: 1680px; table-layout: fixed; border-collapse: collapse; font-size: .72rem; }
.account-table th { padding: .65rem .75rem; color: var(--fg-3); background: var(--surface-2); font-size: .62rem; font-weight: 600; letter-spacing: .04em; text-align: left; }
.account-table td { padding: .7rem .75rem; color: var(--fg-2); vertical-align: middle; border-top: 1px solid var(--hairline); }
.account-table tbody tr { transition: background-color .15s ease; }
.account-table tbody tr:hover { background: var(--hover); }
.account-table th:nth-child(1){width:3%}.account-table th:nth-child(2){width:15%}.account-table th:nth-child(3){width:7%}.account-table th:nth-child(4){width:15%}.account-table th:nth-child(5){width:14%}.account-table th:nth-child(6){width:9%}.account-table th:nth-child(7){width:15%}.account-table th:nth-child(8){width:7%}.account-table th:nth-child(9){width:6%}.account-table th:nth-child(10){width:7%}.account-table th:nth-child(11){width:10%}
.check-cell{text-align:center!important}.chk{width:.9rem;height:.9rem;accent-color:rgb(124 58 237)}
.account-cell strong,.account-cell span { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.account-cell strong { color: var(--fg); font-size: .74rem; font-weight: 600; }.account-cell span { margin-top: .25rem; color: var(--fg-3); font-size: .62rem; }
.route-summary{display:flex;align-items:center;gap:.35rem;color:var(--fg-3);font-size:.62rem}.route-summary>span{font-size:1rem;transition:transform .15s}.route-summary b{color:var(--fg);font-size:.72rem}
.route-preview { display: flex; align-items: center; gap: .25rem; min-width: 0; margin-top: .35rem; overflow: hidden; }
.route-preview span { flex: none; max-width: 7.5rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; border-radius: 999px; padding: .13rem .38rem; color: rgb(5 150 105); background: rgb(16 185 129 / .1); font: 500 .55rem ui-monospace,SFMono-Regular,monospace; }
.quota-line { display:flex;align-items:center;justify-content:space-between;gap:.5rem;font-size:.58rem;color:var(--fg-3) }.quota-line span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.quota-line b{flex:none;color:var(--fg-2);font-weight:600}
.quota-bar { height:.25rem;margin-top:.4rem;border-radius:999px;overflow:hidden;background:var(--surface-2) }.quota-bar span{display:block;height:100%;border-radius:inherit;background:linear-gradient(90deg,rgb(139 92 246),rgb(34 211 238))}
.quota-cell small { display:block;margin-top:.3rem;color:var(--fg-faint);font-size:.56rem }
.job-count { margin-bottom:.3rem;color:var(--fg-3);font-size:.58rem }
.stats-cell div{color:var(--fg-3);font-size:.62rem}.stats-cell b{color:var(--fg);font-size:.78rem}.stats-cell small,.time-cell small{display:block;margin-top:.28rem;color:var(--fg-faint);font-size:.56rem}.stats-cell .ok{color:rgb(16 185 129)}.stats-cell .bad{color:rgb(244 63 94)}.time-cell div{color:var(--fg-2);font-size:.59rem;white-space:nowrap}
.detail-row:hover{background:transparent!important}.detail-row>td{padding:0 1rem 1rem!important}.detail-grid{display:grid;grid-template-columns:1fr 1fr 1fr;gap:1rem;border-radius:.7rem;padding:1rem;background:var(--surface-2);box-shadow:inset 0 0 0 1px var(--hairline)}.detail-grid h4{margin-bottom:.6rem;color:var(--fg);font-size:.66rem;font-weight:600}.route-list,.bucket-list{display:grid;gap:.4rem}.route-list label,.bucket-list>div{display:flex;align-items:center;gap:.75rem;border-radius:.55rem;padding:.55rem .65rem;background:var(--surface)}.route-list label>span{display:flex;min-width:0;flex:1;flex-direction:column}.route-list code,.bucket-list code{color:var(--fg-2);font-size:.6rem}.route-list small,.bucket-list small{margin-top:.2rem;color:var(--fg-faint);font-size:.53rem}.bucket-list>div{align-items:flex-start;flex-direction:column}.bucket-list b{color:var(--fg-2);font-size:.62rem}.route-list p,.bucket-list p{color:var(--fg-3);font-size:.62rem}.health-detail{display:grid;grid-template-columns:1fr 1fr;gap:.4rem}.health-detail>div{display:flex;align-items:center;justify-content:space-between;gap:.5rem;border-radius:.55rem;padding:.65rem;background:var(--surface);font-size:.6rem}.health-detail span{color:var(--fg-3)}.health-detail b{color:var(--fg)}.health-detail .ok{color:rgb(16 185 129)}.health-detail .bad{color:rgb(244 63 94)}.health-detail .warn{color:rgb(245 158 11)}@media(max-width:1200px){.detail-grid{grid-template-columns:1fr 1fr}.detail-grid section:last-child{grid-column:1/-1}}@media(max-width:900px){.detail-grid{grid-template-columns:1fr}.detail-grid section:last-child{grid-column:auto}}
.limit-flags{display:flex;flex-direction:column;align-items:flex-start;gap:.2rem;margin-top:.3rem}.limit-flags span{border-radius:999px;padding:.12rem .38rem;color:rgb(252 211 77);background:rgb(245 158 11 / .1);font-size:.52rem;white-space:nowrap}
.compact-input { width:4.25rem;border:1px solid var(--hairline);border-radius:.45rem;padding:.3rem .45rem;color:var(--fg);background:var(--surface);font-size:.68rem;outline:none }
.compact-input:focus { border-color:rgb(139 92 246 / .65);box-shadow:0 0 0 2px rgb(139 92 246 / .12) }
.empty-cell { height:10rem;text-align:center;color:var(--fg-3)!important }
.pagination { display:flex;align-items:center;gap:.25rem }.pagination button{min-width:1.8rem;height:1.8rem;padding:0 .5rem;border-radius:.45rem;color:var(--fg-2);background:var(--surface-2);box-shadow:inset 0 0 0 1px var(--hairline);font-size:.65rem}.pagination button:hover:not(:disabled){color:var(--fg);background:var(--hover)}.pagination button.on{color:white;background:rgb(124 58 237);box-shadow:none}.pagination button:disabled{opacity:.35}.pagination span{padding:0 .2rem;color:var(--fg-faint)}
.modal-bg { position:fixed;inset:0;z-index:50;display:grid;place-items:center;padding:1rem;background:rgb(0 0 0 / .65);backdrop-filter:blur(6px) }.modal-card{width:100%;max-width:42rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .7);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .45),inset 0 0 0 1px rgb(255 255 255 / .08)}
</style>
