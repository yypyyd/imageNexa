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
const overview = ref({})

const totals = computed(() => ({
  models: models.value.filter((m) => m.enabled !== false).length,
  routes: models.value.reduce((n, m) => n + (m.routes || []).filter((r) => r.enabled !== false).length, 0),
  accounts: accounts.value.filter((a) => ['active', 'enabled', 'healthy'].includes(a.status || 'active')).length,
  keys: credentials.value.filter((k) => k.enabled !== false && k.status !== 'revoked').length,
}))
const last24 = computed(() => overview.value.last_24h || {})
const hourly = computed(() => overview.value.hourly || [])
const hourMax = computed(() => Math.max(1, ...hourly.value.map((bucket) => Number(bucket.text || 0) + Number(bucket.image || 0) + Number(bucket.video || 0))))
const providerRows = computed(() => Object.entries(overview.value.provider_health || {}).map(([name, counts]) => ({
  name,
  ...counts,
  total: Object.values(counts || {}).reduce((sum, value) => sum + Number(value || 0), 0),
})))
const topModelMax = computed(() => Math.max(1, ...(overview.value.top_models || []).map((item) => Number(item.count || 0))))

function fmtTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString('zh-CN', { hour12: false })
}

function fmtMs(value) {
  const number = Number(value || 0)
  if (!number) return '—'
  return number >= 1000 ? `${(number / 1000).toFixed(1)}s` : `${Math.round(number)}ms`
}

async function load() {
  loading.value = true
  error.value = ''
  const [m, a, k, l, o] = await Promise.all([
    api('/logical-models'),
    api('/accounts?limit=200'),
    api('/api-credentials?limit=200'),
    api('/logs?limit=8'),
    api('/overview'),
  ])
  models.value = listOf(m.data)
  accounts.value = listOf(a.data)
  credentials.value = listOf(k.data)
  logs.value = listOf(l.data)
  overview.value = o.ok ? (o.data || {}) : {}
  error.value = [m, a, k, l, o].find((r) => !r.ok)?.error || ''
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

    <div class="grid grid-cols-2 lg:grid-cols-5 gap-3">
      <div class="ops-metric"><span>近 24h 调用</span><strong>{{ Number(last24.total || 0).toLocaleString('zh-CN') }}</strong><small>文 {{ last24.text || 0 }} · 图 {{ last24.image || 0 }} · 视 {{ last24.video || 0 }}</small></div>
      <div class="ops-metric"><span>成功</span><strong class="text-emerald-300">{{ Number(last24.success || 0).toLocaleString('zh-CN') }}</strong><small>{{ last24.total ? `${Math.round(Number(last24.success || 0) / Number(last24.total) * 100)}% 成功率` : '暂无调用' }}</small></div>
      <div class="ops-metric"><span>失败</span><strong class="text-rose-300">{{ Number(last24.failed || 0).toLocaleString('zh-CN') }}</strong><small>{{ last24.pending || 0 }} 个处理中 / 未知</small></div>
      <div class="ops-metric"><span>平均耗时</span><strong>{{ fmtMs(last24.avg_elapsed_ms) }}</strong><small>仅统计成功请求</small></div>
      <div class="ops-metric"><span>失效账号</span><strong class="text-amber-300">{{ providerRows.reduce((sum, item) => sum + Number(item.dead || 0), 0) }}</strong><small>可在账号页批量清理</small></div>
    </div>

    <div class="grid xl:grid-cols-2 gap-4">
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>Provider 健康</h3><span>可用 / 限额 / 失效 / 总数</span></div>
        <div class="provider-health">
          <div v-for="item in providerRows" :key="item.name"><span class="health-dot" :class="item.active ? 'ok' : (item.total ? 'warn' : 'bad')"></span><b>{{ item.name }}</b><code><i>{{ item.active || 0 }}</i> / {{ item.quota || 0 }} / <em>{{ item.dead || 0 }}</em> / {{ item.total }}</code></div>
        </div>
      </div>
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>24 小时调用趋势</h3><span>文本 · 图片 · 视频</span></div>
        <div class="hour-chart">
          <div v-for="bucket in hourly" :key="bucket.hour" :title="`${fmtTime(bucket.hour)} · 文 ${bucket.text || 0} / 图 ${bucket.image || 0} / 视 ${bucket.video || 0}`" :style="{ height: `${Math.max(3, (Number(bucket.text || 0) + Number(bucket.image || 0) + Number(bucket.video || 0)) / hourMax * 100)}%` }"><i v-if="bucket.text" :style="{flex:bucket.text}"></i><b v-if="bucket.image" :style="{flex:bucket.image}"></b><em v-if="bucket.video" :style="{flex:bucket.video}"></em><span v-if="!bucket.text && !bucket.image && !bucket.video"></span></div>
        </div>
        <div class="chart-axis"><span>-24h</span><span>-12h</span><span>现在</span></div>
      </div>
    </div>

    <div class="grid xl:grid-cols-2 gap-4">
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>热门模型 · 24h</h3></div>
        <div v-if="!(overview.top_models || []).length" class="empty-ops">暂无调用</div>
        <div v-else class="ranking"><div v-for="item in overview.top_models" :key="item.model"><p><code>{{ item.model }}</code><span>{{ item.count }} 次 · {{ fmtMs(item.avg_ms) }}</span></p><i><b :style="{width:`${Number(item.count || 0) / topModelMax * 100}%`}"></b></i></div></div>
      </div>
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>失败原因 · 24h</h3></div>
        <div v-if="!(overview.failures || []).length" class="empty-ops text-emerald-300/70">近 24 小时没有失败</div>
        <div v-else class="failures"><div v-for="item in overview.failures" :key="item.reason"><span></span><p :title="item.reason">{{ item.reason }}</p><b>×{{ item.count }}</b></div></div>
      </div>
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
.ops-metric{display:flex;min-height:6rem;flex-direction:column;justify-content:space-between;border-radius:.8rem;padding:.9rem;background:rgb(255 255 255 / .03);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .06)}.ops-metric>span{font-size:.62rem;color:rgb(255 255 255 / .38)}.ops-metric strong{font-size:1.35rem;color:rgb(255 255 255 / .88)}.ops-metric small{font-size:.57rem;color:rgb(255 255 255 / .3)}
.panel-title{display:flex;align-items:center;justify-content:space-between;gap:1rem;border-bottom:1px solid rgb(255 255 255 / .06);padding:.8rem 1rem}.panel-title h3{font-size:.75rem;font-weight:600;color:rgb(255 255 255 / .82)}.panel-title span{font-size:.58rem;color:rgb(255 255 255 / .3)}
.provider-health{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:.2rem;padding:.7rem}.provider-health>div{display:grid;grid-template-columns:auto minmax(0,1fr) auto;align-items:center;gap:.5rem;border-radius:.55rem;padding:.55rem;background:rgb(255 255 255 / .025);font-size:.62rem}.provider-health b{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;text-transform:capitalize;color:rgb(255 255 255 / .68)}.provider-health code{font-size:.58rem;color:rgb(255 255 255 / .35)}.provider-health code i{color:rgb(110 231 183);font-style:normal}.provider-health code em{color:rgb(253 164 175);font-style:normal}.health-dot{width:.42rem;height:.42rem;border-radius:999px;background:rgb(244 63 94)}.health-dot.ok{background:rgb(52 211 153)}.health-dot.warn{background:rgb(251 191 36)}
.hour-chart{display:flex;height:10rem;align-items:flex-end;gap:3px;padding:1rem 1rem .3rem}.hour-chart>div{display:flex;min-height:3px;flex:1;flex-direction:column;justify-content:flex-end;overflow:hidden;border-radius:.18rem .18rem 0 0;background:rgb(255 255 255 / .05)}.hour-chart i{background:rgb(56 189 248 / .8)}.hour-chart b{background:rgb(129 140 248 / .8)}.hour-chart em{background:rgb(232 121 249 / .8)}.hour-chart span{height:100%;background:rgb(255 255 255 / .04)}.chart-axis{display:flex;justify-content:space-between;padding:0 1rem .7rem;font-size:.55rem;color:rgb(255 255 255 / .25)}
.empty-ops{padding:2.5rem;text-align:center;font-size:.68rem;color:rgb(255 255 255 / .3)}.ranking{display:grid;gap:.65rem;padding:1rem}.ranking p{display:flex;justify-content:space-between;gap:1rem;font-size:.6rem}.ranking p code{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:rgb(255 255 255 / .65)}.ranking p span{flex:none;color:rgb(255 255 255 / .32)}.ranking>div>i{display:block;height:.28rem;margin-top:.35rem;overflow:hidden;border-radius:999px;background:rgb(255 255 255 / .05)}.ranking>div>i b{display:block;height:100%;border-radius:inherit;background:linear-gradient(90deg,rgb(139 92 246),rgb(217 70 239))}.failures{display:grid;gap:.2rem;padding:.7rem}.failures>div{display:flex;align-items:center;gap:.55rem;border-radius:.5rem;padding:.55rem;background:rgb(255 255 255 / .025);font-size:.6rem}.failures span{width:.35rem;height:.35rem;flex:none;border-radius:999px;background:rgb(251 113 133)}.failures p{min-width:0;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:rgb(255 255 255 / .55)}.failures b{color:rgb(253 164 175)}
@media(max-width:640px){.provider-health{grid-template-columns:1fr}}
</style>
