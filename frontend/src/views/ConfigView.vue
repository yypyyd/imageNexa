<script setup>
import { onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, apiURL, jsonBody } from '../api'

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

async function load() {
  loading.value = true
  const response = await api('/settings')
  if (response.ok) {
    Object.assign(settings, response.data?.data || response.data || {})
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
  }))
  if (response.ok) {
    Object.assign(settings, response.data?.data || response.data || {})
    saved.value = true
    setTimeout(() => { saved.value = false }, 2000)
  } else error.value = response.error
  saving.value = false
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
</style>
