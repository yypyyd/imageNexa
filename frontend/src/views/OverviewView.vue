<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, apiURL, listOf } from '../api'
import { ACCOUNT_PROVIDERS } from '../models'

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
const providerRows = computed(() => Object.entries(overview.value.provider_health || {}).filter(([name]) => ACCOUNT_PROVIDERS.includes(name)).map(([name, counts]) => ({
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
      <div class="page-head">
        <p class="kicker">OVERVIEW / 01</p>
        <h2 class="page-title">运行概况</h2>
        <p class="page-sub">按渠道拆分的模型目录、账号额度和下游 API Key 的实时摘要。</p>
      </div>
      <button class="btn-soft" :disabled="loading" @click="load"><Icon name="refresh" class="w-3.5 h-3.5" />刷新</button>
    </div>

    <p v-if="error" class="alert">{{ error }}</p>

    <div class="grid grid-cols-2 xl:grid-cols-5 gap-4">
      <div class="stat">
        <div class="stat-chip" :style="ready === false ? 'color: var(--bad); background: color-mix(in srgb, var(--bad) 10%, transparent)' : ''">
          <Icon name="spark" class="w-4.5 h-4.5" />
        </div>
        <span class="stat-label">就绪状态</span>
        <strong class="stat-num" :style="{ color: ready === null ? 'var(--fg-3)' : (ready ? 'var(--ok)' : 'var(--bad)') }">{{ ready === null ? '检查中' : (ready ? '正常' : '异常') }}</strong>
      </div>
      <div class="stat">
        <div class="stat-chip"><Icon name="models" class="w-4.5 h-4.5" /></div>
        <span class="stat-label">启用模型</span>
        <strong class="stat-num">{{ totals.models }}</strong>
      </div>
      <div class="stat">
        <div class="stat-chip"><Icon name="plug" class="w-4.5 h-4.5" /></div>
        <span class="stat-label">启用路由</span>
        <strong class="stat-num">{{ totals.routes }}</strong>
      </div>
      <div class="stat">
        <div class="stat-chip"><Icon name="accounts" class="w-4.5 h-4.5" /></div>
        <span class="stat-label">可用账号</span>
        <strong class="stat-num">{{ totals.accounts }}</strong>
      </div>
      <div class="stat">
        <div class="stat-chip"><Icon name="shield" class="w-4.5 h-4.5" /></div>
        <span class="stat-label">服务 Key</span>
        <strong class="stat-num">{{ totals.keys }}</strong>
      </div>
    </div>

    <div class="grid grid-cols-2 lg:grid-cols-5 gap-4">
      <div class="ops"><span>近 24h 调用</span><strong class="mono">{{ Number(last24.total || 0).toLocaleString('zh-CN') }}</strong><small>文 {{ last24.text || 0 }} · 图 {{ last24.image || 0 }} · 视 {{ last24.video || 0 }}</small></div>
      <div class="ops"><span>成功</span><strong class="mono" style="color: var(--ok)">{{ Number(last24.success || 0).toLocaleString('zh-CN') }}</strong><small>{{ last24.total ? `${Math.round(Number(last24.success || 0) / Number(last24.total) * 100)}% 成功率` : '暂无调用' }}</small></div>
      <div class="ops"><span>失败</span><strong class="mono" style="color: var(--bad)">{{ Number(last24.failed || 0).toLocaleString('zh-CN') }}</strong><small>{{ last24.pending || 0 }} 个处理中 / 未知</small></div>
      <div class="ops"><span>平均耗时</span><strong class="mono">{{ fmtMs(last24.avg_elapsed_ms) }}</strong><small>仅统计成功请求</small></div>
      <div class="ops"><span>失效账号</span><strong class="mono" style="color: var(--warn)">{{ providerRows.reduce((sum, item) => sum + Number(item.dead || 0), 0) }}</strong><small>可在账号页批量清理</small></div>
    </div>

    <div class="grid xl:grid-cols-2 gap-4">
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>Provider 健康</h3><span>可用 / 限额 / 失效 / 总数</span></div>
        <div class="provider-health">
          <div v-for="item in providerRows" :key="item.name">
            <span class="dot" :class="item.active ? 'ok' : (item.total ? 'warn' : 'bad')"></span>
            <b>{{ item.name }}</b>
            <code class="mono"><i>{{ item.active || 0 }}</i> / {{ item.quota || 0 }} / <em>{{ item.dead || 0 }}</em> / {{ item.total }}</code>
          </div>
        </div>
      </div>
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>24 小时调用趋势</h3><span>文本 · 图片 · 视频</span></div>
        <div class="hour-chart">
          <div v-for="bucket in hourly" :key="bucket.hour" :title="`${fmtTime(bucket.hour)} · 文 ${bucket.text || 0} / 图 ${bucket.image || 0} / 视 ${bucket.video || 0}`" :style="{ height: `${Math.max(4, (Number(bucket.text || 0) + Number(bucket.image || 0) + Number(bucket.video || 0)) / hourMax * 100)}%` }"><i v-if="bucket.text" :style="{flex:bucket.text}"></i><b v-if="bucket.image" :style="{flex:bucket.image}"></b><em v-if="bucket.video" :style="{flex:bucket.video}"></em><span v-if="!bucket.text && !bucket.image && !bucket.video"></span></div>
        </div>
        <div class="chart-legend">
          <span><i style="background: var(--kind-text)"></i>文本</span>
          <span><i style="background: var(--kind-image)"></i>图片</span>
          <span><i style="background: var(--kind-video)"></i>视频</span>
          <span class="axis">-24h → 现在</span>
        </div>
      </div>
    </div>

    <div class="grid xl:grid-cols-2 gap-4">
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>热门模型 · 24h</h3></div>
        <div v-if="!(overview.top_models || []).length" class="empty">暂无调用</div>
        <div v-else class="ranking"><div v-for="item in overview.top_models" :key="item.model"><p><code class="mono">{{ item.model }}</code><span>{{ item.count }} 次 · {{ fmtMs(item.avg_ms) }}</span></p><i><b :style="{width:`${Number(item.count || 0) / topModelMax * 100}%`}"></b></i></div></div>
      </div>
      <div class="card overflow-hidden">
        <div class="panel-title"><h3>失败原因 · 24h</h3></div>
        <div v-if="!(overview.failures || []).length" class="empty" style="color: var(--ok)">近 24 小时没有失败</div>
        <div v-else class="failures"><div v-for="item in overview.failures" :key="item.reason"><span></span><p :title="item.reason">{{ item.reason }}</p><b>×{{ item.count }}</b></div></div>
      </div>
    </div>

    <div class="grid xl:grid-cols-[1.3fr_.7fr] gap-4">
      <div class="card overflow-hidden">
        <div class="panel-title">
          <h3>最近调用</h3>
          <router-link to="/admin/logs" class="more-link">查看全部 →</router-link>
        </div>
        <div v-if="loading" class="empty">加载中…</div>
        <div v-else-if="!logs.length" class="empty">暂无调用日志</div>
        <div v-else class="log-list">
          <div v-for="item in logs" :key="item.id || item.request_id" class="log-row">
            <div class="min-w-0">
              <div class="flex items-center gap-2.5">
                <span class="mono log-model">{{ item.model || '—' }}</span>
                <span class="pill" :class="item.status === 'succeeded' || item.status === 'success' ? 'ok' : (item.status === 'failed' ? 'bad' : '')">{{ item.status || '—' }}</span>
              </div>
              <div class="mt-1 text-[10.5px] text-[color:var(--fg-faint)] truncate">{{ item.credential?.name || item.api_credential_name || item.key_preview || '未知 Key' }} · {{ item.request_id || item.id }}</div>
            </div>
            <time class="mono text-[10.5px] text-[color:var(--fg-faint)] whitespace-nowrap">{{ fmtTime(item.created_at) }}</time>
          </div>
        </div>
      </div>

      <div class="card p-5">
        <h3 class="panel-heading">快速检查</h3>
        <div class="mt-3 grid gap-2">
          <router-link v-for="link in [
            ['账号额度与冷却','/admin/accounts','plug'],
            ['模型路由能力','/admin/models','models'],
            ['API Key 并发限制','/admin/api-keys','shield'],
            ['调用文档','/admin/docs','book'],
          ]" :key="link[1]" :to="link[1]" class="quick">
            <span class="quick-chip"><Icon :name="link[2]" class="w-4 h-4" /></span>
            <span>{{ link[0] }}</span>
            <span class="arrow">→</span>
          </router-link>
        </div>
      </div>
    </div>
  </section>
</template>

<style scoped>
.alert { border: 1px solid rgb(220 38 38 / .3); background: rgb(220 38 38 / .07); color: var(--bad); padding: .75rem 1rem; font-size: 12px; border-radius: 12px; }

/* stat cards with icon chips */
.stat {
  display: flex; flex-direction: column; gap: 10px;
  padding: 18px;
  background: var(--surface);
  border: 1px solid var(--hairline);
  border-radius: 16px;
  box-shadow: var(--shadow-card);
}
.stat-chip {
  width: 38px; height: 38px; border-radius: 12px;
  display: grid; place-items: center;
  color: var(--accent);
  background: var(--accent-soft);
}
.stat-chip svg { width: 18px; height: 18px; }
.stat-label { font-size: 12px; color: var(--fg-3); }
.stat-num { font-size: 26px; line-height: 1; font-weight: 700; letter-spacing: -0.02em; color: var(--fg); font-variant-numeric: tabular-nums; }

/* slim ops metrics */
.ops {
  display: flex; min-height: 5.2rem; flex-direction: column; justify-content: space-between; gap: 4px;
  padding: 16px; border-radius: 16px;
  background: var(--surface);
  border: 1px solid var(--hairline);
  box-shadow: var(--shadow-card);
}
.ops > span { font-size: 11.5px; color: var(--fg-3); }
.ops strong { font-size: 21px; font-weight: 700; letter-spacing: -0.01em; color: var(--fg); }
.ops small { font-size: 10.5px; color: var(--fg-faint); }

.panel-title {
  display: flex; align-items: center; justify-content: space-between; gap: 1rem;
  border-bottom: 1px solid var(--hairline-soft);
  padding: 16px 20px;
}
.panel-title h3 { font-size: 13.5px; font-weight: 650; letter-spacing: -0.01em; color: var(--fg); margin: 0; }
.panel-title span { font-size: 11px; color: var(--fg-faint); }
.panel-heading { margin: 0; font-size: 13.5px; font-weight: 650; color: var(--fg); }
.more-link { font-size: 12px; color: var(--accent); }
.more-link:hover { text-decoration: underline; }

/* round status dots */
.dot { width: 8px; height: 8px; flex: none; border-radius: 999px; background: var(--fg-faint); }
.dot.ok { background: var(--ok); box-shadow: 0 0 0 3px color-mix(in srgb, var(--ok) 16%, transparent); }
.dot.warn { background: var(--warn); box-shadow: 0 0 0 3px color-mix(in srgb, var(--warn) 16%, transparent); }
.dot.bad { background: var(--bad); box-shadow: 0 0 0 3px color-mix(in srgb, var(--bad) 16%, transparent); }

.provider-health { display: grid; grid-template-columns: repeat(2, minmax(0,1fr)); gap: 8px; padding: 16px; }
.provider-health > div {
  display: grid; grid-template-columns: auto minmax(0,1fr) auto; align-items: center; gap: 10px;
  border-radius: 12px; padding: 11px 14px;
  background: var(--surface-2); font-size: 12.5px;
}
.provider-health b { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; text-transform: capitalize; color: var(--fg); font-weight: 600; }
.provider-health code { font-size: 10.5px; color: var(--fg-faint); }
.provider-health code i { color: var(--ok); font-style: normal; }
.provider-health code em { color: var(--bad); font-style: normal; }

.hour-chart { display: flex; height: 9.5rem; align-items: flex-end; gap: 4px; padding: 18px 20px 6px; }
.hour-chart > div {
  display: flex; min-height: 4px; flex: 1; flex-direction: column; justify-content: flex-end;
  overflow: hidden; border-radius: 4px 4px 2px 2px; background: var(--surface-2);
}
.hour-chart i { background: var(--kind-text); opacity: .9; }
.hour-chart b { background: var(--kind-image); opacity: .9; }
.hour-chart em { background: var(--kind-video); opacity: .9; }
.hour-chart span { height: 100%; background: transparent; }
.chart-legend { display: flex; align-items: center; gap: 14px; padding: 0 20px 16px; font-size: 10.5px; color: var(--fg-3); }
.chart-legend span { display: inline-flex; align-items: center; gap: 6px; }
.chart-legend i { width: 8px; height: 8px; border-radius: 3px; }
.chart-legend .axis { margin-left: auto; color: var(--fg-faint); font-family: var(--font-mono); }

.empty { padding: 2.5rem; text-align: center; font-size: 12px; color: var(--fg-faint); }
.ranking { display: grid; gap: 14px; padding: 16px 20px; }
.ranking p { display: flex; justify-content: space-between; gap: 1rem; font-size: 11px; margin: 0; }
.ranking p code { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--fg); font-size: 11px; }
.ranking p span { flex: none; color: var(--fg-faint); font-family: var(--font-mono); font-size: 10.5px; }
.ranking > div > i { display: block; height: 6px; margin-top: 6px; overflow: hidden; border-radius: 999px; background: var(--surface-2); }
.ranking > div > i > b { display: block; height: 100%; border-radius: inherit; background: linear-gradient(90deg, #06b6d4, #6366f1); }

.failures { display: grid; gap: 6px; padding: 14px 16px; }
.failures > div { display: flex; align-items: center; gap: 10px; border-radius: 10px; padding: 10px 12px; background: var(--surface-2); font-size: 11px; }
.failures span { width: 7px; height: 7px; flex: none; border-radius: 999px; background: var(--bad); }
.failures p { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--fg-2); margin: 0; }
.failures b { color: var(--bad); font-family: var(--font-mono); font-size: 10.5px; }

.log-list { display: flex; flex-direction: column; }
.log-row {
  display: grid; grid-template-columns: minmax(0,1fr) auto; gap: 12px; align-items: center;
  padding: 12px 20px;
}
.log-row + .log-row { border-top: 1px solid var(--hairline-soft); }
.log-model { font-size: 11.5px; color: var(--fg); font-weight: 500; }
.pill {
  font-family: var(--font-mono); font-size: 9.5px; letter-spacing: .04em;
  border-radius: 999px; padding: 3px 9px;
  color: var(--fg-3); background: var(--surface-2);
}
.pill.ok { color: var(--ok); background: color-mix(in srgb, var(--ok) 12%, transparent); }
.pill.bad { color: var(--bad); background: color-mix(in srgb, var(--bad) 12%, transparent); }

.quick {
  display: flex; align-items: center; gap: 12px;
  border-radius: 12px; padding: 11px 13px;
  color: var(--fg-2); font-size: 13px; font-weight: 500;
  border: 1px solid transparent;
  transition: background-color .15s ease, border-color .15s ease;
}
.quick:hover { background: var(--hover); border-color: var(--hairline-soft); }
.quick-chip {
  width: 32px; height: 32px; border-radius: 10px; flex: none;
  display: grid; place-items: center;
  color: var(--accent); background: var(--accent-soft);
}
.quick .arrow { margin-left: auto; color: var(--fg-faint); transition: color .15s ease, transform .15s ease; }
.quick:hover .arrow { color: var(--accent); transform: translateX(2px); }

@media(max-width:640px){ .provider-health { grid-template-columns: 1fr; } }
</style>
