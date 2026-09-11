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

const visibleProviders = computed(() => Object.keys(providers).filter((pool) => ACCOUNT_PROVIDERS.includes(pool)).sort())

function providerLabel(pool) {
  return PROVIDER_LABELS[pool] || pool[0].toUpperCase() + pool.slice(1)
}

function applySettings(data) {
  const { providers_enabled: enabled, ...rest } = data || {}
  Object.assign(settings, rest)
  if (enabled && typeof enabled === 'object') {
    for (const key of Object.keys(providers)) delete providers[key]
    Object.assign(providers, enabled)
  }
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
  <section class="space-y-5 max-w-5xl">
    <div class="flex justify-between items-start gap-4">
      <div><h2 class="text-xl font-semibold text-white/90">系统设置</h2><p class="mt-1 text-xs text-white/40">2API 服务地址、上游网络和数据保留策略。</p></div>
      <button class="btn-primary" :disabled="saving || loading" @click="save"><Icon name="check" class="w-3.5 h-3.5" />{{ saving ? '保存中…' : (saved ? '已保存' : '保存设置') }}</button>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div class="grid md:grid-cols-2 gap-3">
      <div class="health"><div><div class="health-label">Liveness</div><code>/health/live</code></div><span :class="health.live ? 'ok' : 'bad'">{{ health.live === null ? '检查中' : (health.live ? '正常' : '异常') }}</span></div>
      <div class="health"><div><div class="health-label">Readiness</div><code>/health/ready</code></div><span :class="health.ready ? 'ok' : 'bad'">{{ health.ready === null ? '检查中' : (health.ready ? '就绪' : '未就绪') }}</span></div>
    </div>

    <form class="space-y-4" @submit.prevent="save">
      <div class="card p-5 space-y-4">
        <div><h3 class="section-heading">公开 API</h3><p class="section-desc">用于文档示例和生成短期签名媒体 URL。</p></div>
        <label class="block"><span class="label">对外 Base URL</span><input v-model="settings.public_base_url" class="field mt-1.5 font-mono text-xs" placeholder="https://api.example.com" /><span class="hint">不要包含 <code>/v1</code> 或尾部斜杠。</span></label>
      </div>

      <div class="card p-5 space-y-4">
        <div><h3 class="section-heading">上游网络</h3><p class="section-desc">为支持的 Provider 配置统一出站代理。</p></div>
        <label class="block"><span class="label">出站代理</span><input v-model="settings.outbound_proxy" class="field mt-1.5 font-mono text-xs" placeholder="http://127.0.0.1:7890（留空为直连）" /><span class="hint">修改后仅影响新建连接；账号调度状态不会被重置。</span></label>
      </div>

      <div class="card p-5 space-y-4">
        <div class="flex items-start justify-between gap-3">
          <div><h3 class="section-heading">平台启用</h3><p class="section-desc">关闭后调度器不再从该平台的号池选号；已有 route 和账号配置保留不变，重新开启即恢复。</p></div>
          <span v-if="disabledCount()" class="badge-off">{{ disabledCount() }} 个已停用</span>
        </div>
        <div class="provider-grid">
          <label v-for="pool in visibleProviders" :key="pool" class="provider-row" :class="!providers[pool] && 'off'">
            <span><strong>{{ providerLabel(pool) }}</strong><code>{{ pool }}</code></span>
            <button type="button" class="switch" :class="providers[pool] && 'on'" :aria-label="`${providers[pool] ? '停用' : '启用'} ${providerLabel(pool)}`" :title="providers[pool] ? '点击停用该平台' : '点击启用该平台'" @click="toggleProvider(pool)"><span></span></button>
          </label>
          <p v-if="!visibleProviders.length && !loading" class="hint">未获取到平台列表。</p>
        </div>
        <span class="hint">若某个模型的全部平台都被关闭，该模型的请求会返回 <code>provider_disabled</code>，而不会白跑上游重试。</span>
      </div>

      <div class="card p-5 space-y-4">
        <div><h3 class="section-heading">数据保留</h3><p class="section-desc">清理任务必须保留 API Key 归属与必要的审计快照。</p></div>
        <div class="grid sm:grid-cols-2 gap-3">
          <label><span class="label">调用日志保留天数</span><input v-model.number="settings.logs_retention_days" type="number" min="1" max="3650" class="field mt-1.5" /></label>
          <label><span class="label">成品保留天数</span><input v-model.number="settings.artifacts_retention_days" type="number" min="1" max="3650" class="field mt-1.5" /></label>
        </div>
      </div>
    </form>

    <div class="card p-5">
      <h3 class="section-heading">管理员会话安全</h3>
      <div class="mt-3 grid sm:grid-cols-3 gap-2 text-[11px]">
        <div class="security"><strong>HttpOnly Cookie</strong><span>已启用</span></div><div class="security"><strong>Origin + CSRF</strong><span>写请求必须验证</span></div><div class="security"><strong>Bearer Token</strong><span>仅下游 /v1 API 使用</span></div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.notice{border-radius:.7rem;padding:.7rem .9rem;font-size:.72rem;color:rgb(253 164 175);background:rgb(244 63 94 / .09)}.health{display:flex;align-items:center;justify-content:space-between;padding:1rem;border-radius:.8rem;background:rgb(255 255 255 / .035);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .065)}.health-label{font-size:.65rem;text-transform:uppercase;letter-spacing:.08em;color:rgb(255 255 255 / .35)}.health code{font-size:.68rem;color:rgb(255 255 255 / .65)}.health span{font-size:.66rem;border-radius:999px;padding:.2rem .5rem}.health .ok{color:rgb(110 231 183);background:rgb(16 185 129 / .1)}.health .bad{color:rgb(253 164 175);background:rgb(244 63 94 / .1)}.section-heading{font-size:.85rem;font-weight:600;color:rgb(255 255 255 / .85)}.section-desc{margin-top:.3rem;font-size:.68rem;color:rgb(255 255 255 / .35)}.label{display:block;font-size:.68rem;font-weight:600;color:rgb(255 255 255 / .5)}.hint{display:block;margin-top:.35rem;font-size:.62rem;color:rgb(255 255 255 / .28)}.security{display:flex;flex-direction:column;gap:.35rem;padding:.75rem;border-radius:.65rem;background:rgb(255 255 255 / .03)}.security strong{color:rgb(255 255 255 / .7)}.security span{color:rgb(110 231 183 / .7)}
.badge-off{flex:none;font-size:.62rem;border-radius:999px;padding:.2rem .55rem;color:rgb(253 164 175);background:rgb(244 63 94 / .1)}
.provider-grid{display:grid;gap:.5rem;grid-template-columns:repeat(auto-fill,minmax(14rem,1fr))}
.provider-row{display:flex;align-items:center;justify-content:space-between;gap:.75rem;padding:.65rem .8rem;border-radius:.65rem;background:rgb(255 255 255 / .035);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .065);cursor:pointer;transition:background .15s}
.provider-row:hover{background:rgb(255 255 255 / .05)}.provider-row.off{opacity:.6}
.provider-row > span{display:flex;flex-direction:column;gap:.15rem;min-width:0}.provider-row strong{font-size:.72rem;color:rgb(255 255 255 / .8)}.provider-row code{font-size:.6rem;color:rgb(255 255 255 / .35)}
.switch{position:relative;width:2.2rem;height:1.25rem;flex:none;border-radius:999px;background:rgb(255 255 255 / .12)}.switch span{position:absolute;width:.95rem;height:.95rem;left:.15rem;top:.15rem;border-radius:999px;background:white;transition:transform .15s}.switch.on{background:rgb(16 185 129 / .7)}.switch.on span{transform:translateX(.95rem)}
</style>
