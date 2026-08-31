<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, apiURL, listOf } from '../api'

const loading = ref(true)
const error = ref('')
const models = ref([])
const accounts = ref([])
const credentials = ref([])
const logs = ref([])
const ready = ref(null)

const totals = computed(() => ({
  models: models.value.filter((m) => m.enabled !== false).length,
  routes: models.value.reduce((n, m) => n + (m.routes || []).filter((r) => r.enabled !== false).length, 0),
  accounts: accounts.value.filter((a) => ['active', 'enabled', 'healthy'].includes(a.status || 'active')).length,
  keys: credentials.value.filter((k) => k.enabled !== false && k.status !== 'revoked').length,
}))

function fmtTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

async function load() {
  loading.value = true
  error.value = ''
  const [m, a, k, l] = await Promise.all([
    api('/logical-models'),
    api('/accounts?limit=200'),
    api('/api-credentials?limit=200'),
    api('/logs?limit=8'),
  ])
  models.value = listOf(m.data)
  accounts.value = listOf(a.data)
  credentials.value = listOf(k.data)
  logs.value = listOf(l.data)
  error.value = [m, a, k, l].find((r) => !r.ok)?.error || ''
  try {
    const response = await fetch(apiURL('/health/ready'), { headers: { Accept: 'application/json' } })
    ready.value = response.ok
  } catch { ready.value = false }
  loading.value = false
}

onMounted(load)
</script>

<template>
  <section class="space-y-5">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">2API 运行概况</h2>
        <p class="mt-1 text-xs text-white/40">统一模型路由、账号额度和下游 API Key 的实时摘要。</p>
      </div>
      <button class="btn-soft" :disabled="loading" @click="load"><Icon name="refresh" class="w-3.5 h-3.5" />刷新</button>
    </div>

    <p v-if="error" class="rounded-xl bg-rose-500/10 ring-1 ring-rose-400/20 px-4 py-3 text-xs text-rose-400">{{ error }}</p>

    <div class="grid grid-cols-2 xl:grid-cols-5 gap-3">
      <div class="metric"><span class="metric-label">就绪状态</span><strong :class="ready ? 'text-emerald-300' : 'text-rose-300'">{{ ready === null ? '检查中' : (ready ? '正常' : '异常') }}</strong></div>
      <div class="metric"><span class="metric-label">启用模型</span><strong>{{ totals.models }}</strong></div>
      <div class="metric"><span class="metric-label">启用路由</span><strong>{{ totals.routes }}</strong></div>
      <div class="metric"><span class="metric-label">可用账号</span><strong>{{ totals.accounts }}</strong></div>
      <div class="metric"><span class="metric-label">服务 Key</span><strong>{{ totals.keys }}</strong></div>
    </div>

    <div class="grid xl:grid-cols-[1.3fr_.7fr] gap-4">
      <div class="card overflow-hidden">
        <div class="px-5 py-4 border-b border-white/[0.06] flex items-center justify-between">
          <h3 class="text-sm font-semibold text-white/85">最近调用</h3>
          <router-link to="/admin/logs" class="text-xs text-violet-300 hover:text-violet-200">查看全部</router-link>
        </div>
        <div v-if="loading" class="py-16 text-center text-xs text-white/35">加载中…</div>
        <div v-else-if="!logs.length" class="py-16 text-center text-xs text-white/35">暂无调用日志</div>
        <div v-else class="divide-y divide-white/[0.05]">
          <div v-for="item in logs" :key="item.id || item.request_id" class="px-5 py-3 grid grid-cols-[minmax(0,1fr)_auto] gap-3 items-center">
            <div class="min-w-0">
              <div class="flex items-center gap-2">
                <span class="font-mono text-xs text-white/85 truncate">{{ item.model || '—' }}</span>
                <span class="status-dot" :class="item.status === 'succeeded' || item.status === 'success' ? 'ok' : (item.status === 'failed' ? 'bad' : '')">{{ item.status || '—' }}</span>
              </div>
              <div class="mt-1 text-[10px] text-white/35 truncate">{{ item.credential?.name || item.api_credential_name || item.key_preview || '未知 Key' }} · {{ item.request_id || item.id }}</div>
            </div>
            <time class="text-[10px] text-white/30 whitespace-nowrap">{{ fmtTime(item.created_at) }}</time>
          </div>
        </div>
      </div>

      <div class="card p-5 space-y-4">
        <h3 class="text-sm font-semibold text-white/85">快速检查</h3>
        <router-link v-for="link in [
          ['账号额度与冷却','/admin/accounts','plug'],
          ['模型路由能力','/admin/models','models'],
          ['API Key 并发限制','/admin/api-keys','shield'],
          ['调用文档','/admin/docs','book'],
        ]" :key="link[1]" :to="link[1]" class="flex items-center gap-3 rounded-xl bg-white/[0.035] ring-1 ring-white/[0.06] px-3.5 py-3 hover:bg-white/[0.07] transition-colors">
          <Icon :name="link[2]" class="w-4 h-4 text-violet-300" />
          <span class="text-xs text-white/70">{{ link[0] }}</span>
          <span class="ml-auto text-white/25">→</span>
        </router-link>
      </div>
    </div>
  </section>
</template>

<style scoped>
.metric { min-height: 7rem; display: flex; flex-direction: column; justify-content: space-between; padding: 1rem; border-radius: .85rem; background: rgb(255 255 255 / .035); box-shadow: inset 0 0 0 1px rgb(255 255 255 / .065); }
.metric-label { font-size: .65rem; color: rgb(255 255 255 / .4); letter-spacing: .08em; }
.metric strong { font-size: 1.65rem; line-height: 1; font-weight: 650; color: rgb(255 255 255 / .9); }
.status-dot { font-size: .62rem; border-radius: 999px; padding: .1rem .4rem; color: rgb(255 255 255 / .5); background: rgb(255 255 255 / .06); }
.status-dot.ok { color: rgb(110 231 183); background: rgb(16 185 129 / .12); }
.status-dot.bad { color: rgb(253 164 175); background: rgb(244 63 94 / .12); }
</style>
