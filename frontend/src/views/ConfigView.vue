<script setup>
import { onMounted, reactive, ref, computed } from 'vue'
import Icon from '../components/Icon.vue'
import { api, apiURL, jsonBody } from '../api'
import { ACCOUNT_PROVIDERS } from '../models'

const loading = ref(true)
const saving = ref(false)
const error = ref('')
const saved = ref(false)
const health = reactive({ live: null, ready: null })
const settings = reactive({
  public_base_url: '',
  outbound_proxy: '',
  outbound_extract_api: '',
  dola_session_api: '',
  provider_proxies: Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, ''])),
  provider_extract_apis: Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, ''])),
  logs_retention_days: 30,
  artifacts_retention_days: 30,
})
// Keyed by provider pool; true means the scheduler may pick accounts from it.
const providers = reactive({})

const PROVIDER_LABELS = {
  adobe: 'Adobe Firefly',
  byteplus: 'BytePlus',
  chatgpt: 'ChatGPT',
  dola: 'Dola (豆包国际版)',
  grok: 'Grok',
}

const PROXY_PROVIDER_ORDER = ['chatgpt', 'grok', 'dola', 'adobe', 'byteplus']
const PROXY_INHERIT_DEFAULT = new Set(['chatgpt', 'grok', 'dola'])

const visibleProviders = computed(() => Object.keys(providers).filter((pool) => ACCOUNT_PROVIDERS.includes(pool)).sort())
const proxyProviders = computed(() => PROXY_PROVIDER_ORDER.filter((pool) => ACCOUNT_PROVIDERS.includes(pool)))
const hasChannelOverrides = computed(() =>
  proxyProviders.value.some((pool) => (settings.provider_extract_apis[pool] || '').trim())
)
const showChannelProxies = ref(false)

function providerLabel(pool) {
  return PROVIDER_LABELS[pool] || pool[0].toUpperCase() + pool.slice(1)
}

function emptyProviderProxies() {
  return Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, '']))
}

function extractHint(pool) {
  if (PROXY_INHERIT_DEFAULT.has(pool)) {
    return '留空则使用上方默认提取 API'
  }
  return '留空为直连，不使用默认提取 API'
}

function applySettings(data) {
  const { providers_enabled: enabled, provider_proxies: proxies, provider_extract_apis: extracts, ...rest } = data || {}
  Object.assign(settings, rest)
  settings.provider_proxies = { ...emptyProviderProxies(), ...(proxies && typeof proxies === 'object' ? proxies : {}) }
  settings.provider_extract_apis = { ...emptyProviderProxies(), ...(extracts && typeof extracts === 'object' ? extracts : {}) }
  if (!(settings.provider_extract_apis.dola || '').trim() && (settings.dola_session_api || '').trim()) {
    settings.provider_extract_apis.dola = settings.dola_session_api
  }
  if (enabled && typeof enabled === 'object') {
    for (const key of Object.keys(providers)) delete providers[key]
    Object.assign(providers, enabled)
  }
  if (hasChannelOverrides.value) showChannelProxies.value = true
}

async function load() {
  loading.value = true
  const response = await api('/settings')
  if (response.ok) {
    applySettings(response.data?.data || response.data || {})
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

async function save() {
  saving.value = true
  saved.value = false
  const response = await api('/settings', jsonBody('PUT', {
    public_base_url: settings.public_base_url.trim(),
    outbound_proxy: settings.outbound_proxy.trim(),
    outbound_extract_api: (settings.outbound_extract_api || '').trim(),
    provider_extract_apis: Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, (settings.provider_extract_apis[pool] || '').trim()])),
    logs_retention_days: Math.max(1, Number(settings.logs_retention_days) || 30),
    artifacts_retention_days: Math.max(1, Number(settings.artifacts_retention_days) || 30),
    providers_enabled: { ...providers },
  }))
  if (response.ok) {
    applySettings(response.data?.data || response.data || {})
    saved.value = true
    setTimeout(() => { saved.value = false }, 2000)
  } else error.value = response.error
  saving.value = false
}

function toggleProvider(pool) {
  providers[pool] = !providers[pool]
}

const disabledCount = () => visibleProviders.value.filter((pool) => !providers[pool]).length

function healthText(value, okLabel, badLabel) {
  if (value === null) return '检查中'
  return value ? okLabel : badLabel
}

function healthClass(value) {
  if (value === null) return 'wait'
  return value ? 'ok' : 'bad'
}

async function checkHealth(name) {
  health[name] = null
  try {
    const response = await fetch(apiURL(`/health/${name}`), { headers: { Accept: 'application/json' } })
    health[name] = response.ok
  } catch { health[name] = false }
}

onMounted(() => { load(); checkHealth('live'); checkHealth('ready') })
</script>

<template>
  <section class="space-y-5">
    <div class="flex justify-between items-start gap-4">
      <div>
        <h2 class="page-title">系统设置</h2>
        <p class="page-sub">对外地址、号池开关、上游出口和日志保留。</p>
      </div>
      <div class="flex flex-wrap items-center justify-end gap-2">
        <span class="health-pill" :class="healthClass(health.live)" :title="'/health/live'">存活 {{ healthText(health.live, '正常', '异常') }}</span>
        <span class="health-pill" :class="healthClass(health.ready)" :title="'/health/ready'">就绪 {{ healthText(health.ready, '就绪', '未就绪') }}</span>
        <button class="btn-primary" :disabled="saving || loading" @click="save"><Icon name="check" class="w-3.5 h-3.5" />{{ saving ? '保存中…' : (saved ? '已保存' : '保存设置') }}</button>
      </div>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <form class="space-y-4" @submit.prevent="save">
      <div class="grid gap-4 xl:grid-cols-2">
        <div class="card p-5 space-y-4">
          <div><h3 class="section-heading">公开 API</h3><p class="section-desc">文档示例和成品链接用的对外地址。</p></div>
          <label class="block"><span class="label">对外 Base URL</span><input v-model="settings.public_base_url" class="field mt-1.5 font-mono text-xs" placeholder="https://api.example.com" /><span class="hint">不要带 <code>/v1</code> 或末尾斜杠。</span></label>
        </div>

        <div class="card p-5 space-y-4">
          <div class="flex items-start justify-between gap-3">
            <div><h3 class="section-heading">平台启用</h3><p class="section-desc">关掉后不再从该号池选号，配置还在，打开即恢复。</p></div>
            <span v-if="disabledCount()" class="badge-off">{{ disabledCount() }} 个已停用</span>
          </div>
          <div class="provider-grid">
            <label v-for="pool in visibleProviders" :key="pool" class="provider-row" :class="!providers[pool] && 'off'">
              <span><strong>{{ providerLabel(pool) }}</strong><code>{{ pool }}</code></span>
              <button type="button" class="switch" :class="providers[pool] && 'on'" :aria-label="`${providers[pool] ? '停用' : '启用'} ${providerLabel(pool)}`" :title="providers[pool] ? '点击停用该平台' : '点击启用该平台'" @click="toggleProvider(pool)"><span></span></button>
            </label>
            <p v-if="!visibleProviders.length && !loading" class="hint">未获取到平台列表。</p>
          </div>
        </div>
      </div>

      <div class="card p-5 space-y-4">
        <div>
          <h3 class="section-heading">上游网络</h3>
          <p class="section-desc">ChatGPT / Grok / Dola 共用默认提取 API，按账号粘性分配出口。Adobe / BytePlus 留空即直连。</p>
        </div>
        <label class="block"><span class="label">默认提取 API</span><input v-model="settings.outbound_extract_api" class="field mt-1.5 font-mono text-xs" placeholder="http://host:port/gen?zone=…&count=64" /><span class="hint">链接里的 count 只是上限，单次最多领 64 条。</span></label>
        <label class="block"><span class="label">回退出站网关</span><input v-model="settings.outbound_proxy" class="field mt-1.5 font-mono text-xs" placeholder="http://user:pass@host:port（可选）" /><span class="hint">提取失败时用。Adobe / BytePlus 不走这项。</span></label>
        <div>
          <button type="button" class="linkish" @click="showChannelProxies = !showChannelProxies">
            {{ showChannelProxies ? '收起各渠道出口' : '填写各渠道单独出口（可选）' }}
            <span v-if="hasChannelOverrides && !showChannelProxies" class="dot">已有覆盖</span>
          </button>
          <div v-if="showChannelProxies" class="channel-grid">
            <label v-for="pool in proxyProviders" :key="pool" class="block">
              <span class="label">{{ providerLabel(pool) }}</span>
              <input v-model="settings.provider_extract_apis[pool]" class="field mt-1.5 font-mono text-xs" :placeholder="extractHint(pool)" />
            </label>
          </div>
        </div>
      </div>

      <div class="card p-5 space-y-4">
        <div><h3 class="section-heading">数据保留</h3><p class="section-desc">过期日志和成品会按天清理；API Key 归属会留下。</p></div>
        <div class="grid sm:grid-cols-2 gap-3">
          <label><span class="label">调用日志保留天数</span><input v-model.number="settings.logs_retention_days" type="number" min="1" max="3650" class="field mt-1.5" /></label>
          <label><span class="label">成品保留天数</span><input v-model.number="settings.artifacts_retention_days" type="number" min="1" max="3650" class="field mt-1.5" /></label>
        </div>
      </div>
    </form>
  </section>
</template>

<style scoped>
.page-title { font-size: 1.25rem; line-height: 1.75rem; font-weight: 600; color: var(--fg); }
.page-sub { margin-top: .25rem; font-size: .75rem; color: var(--fg-3); }
.notice { border-radius: .7rem; padding: .7rem .9rem; font-size: .72rem; color: rgb(190 18 60); background: rgb(244 63 94 / .09); }
.health-pill { font-size: .66rem; border-radius: 999px; padding: .28rem .65rem; background: var(--surface-2); box-shadow: inset 0 0 0 1px var(--hairline); color: var(--fg-3); }
.health-pill.ok { color: rgb(4 120 87); background: rgb(16 185 129 / .12); }
.health-pill.bad { color: rgb(190 18 60); background: rgb(244 63 94 / .1); }
.health-pill.wait { color: var(--fg-3); }
:global(html.dark) .health-pill.ok { color: rgb(110 231 183); }
:global(html.dark) .health-pill.bad { color: rgb(253 164 175); }
:global(html.dark) .notice { color: rgb(253 164 175); }
:global(html.dark) .badge-off { color: rgb(253 164 175); }
.section-heading { font-size: .85rem; font-weight: 600; color: var(--fg); }
.section-desc { margin-top: .3rem; font-size: .68rem; color: var(--fg-3); }
.label { display: block; font-size: .68rem; font-weight: 600; color: var(--fg-3); }
.hint { display: block; margin-top: .35rem; font-size: .62rem; color: var(--fg-faint); }
.hint code { color: var(--fg-2); }
.linkish { display: inline-flex; align-items: center; gap: .45rem; font-size: .72rem; font-weight: 600; color: var(--fg-2); }
.linkish:hover { color: var(--fg); }
.dot { font-size: .62rem; font-weight: 500; color: rgb(180 83 9); }
.channel-grid { display: grid; gap: .85rem; margin-top: .9rem; grid-template-columns: repeat(auto-fill, minmax(16rem, 1fr)); }
.badge-off { flex: none; font-size: .62rem; border-radius: 999px; padding: .2rem .55rem; color: rgb(190 18 60); background: rgb(244 63 94 / .1); }
.provider-grid { display: grid; gap: .5rem; grid-template-columns: repeat(auto-fill, minmax(14rem, 1fr)); }
.provider-row { display: flex; align-items: center; justify-content: space-between; gap: .75rem; padding: .65rem .8rem; border-radius: .65rem; background: var(--surface-2); box-shadow: inset 0 0 0 1px var(--hairline); cursor: pointer; transition: background .15s; }
.provider-row:hover { background: var(--hover); }
.provider-row.off { opacity: .62; }
.provider-row > span { display: flex; flex-direction: column; gap: .15rem; min-width: 0; }
.provider-row strong { font-size: .72rem; color: var(--fg); }
.provider-row code { font-size: .6rem; color: var(--fg-3); }
.switch { position: relative; width: 2.2rem; height: 1.25rem; flex: none; border-radius: 999px; background: color-mix(in srgb, var(--fg) 18%, transparent); }
.switch span { position: absolute; width: .95rem; height: .95rem; left: .15rem; top: .15rem; border-radius: 999px; background: #fff; box-shadow: 0 1px 2px rgb(15 23 42 / .2); transition: transform .15s; }
.switch.on { background: rgb(16 185 129 / .8); }
.switch.on span { transform: translateX(.95rem); }
</style>
