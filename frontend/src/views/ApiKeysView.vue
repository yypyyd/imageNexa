<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, jsonBody, listOf } from '../api'
import { copyText } from '../utils/clipboard'

const credentials = ref([])
const loading = ref(true)
const error = ref('')
const busy = ref('')
const creating = ref(false)
const form = reactive({ name: '', concurrency_limit: 0 })
const revealed = ref(null)
const copied = ref(false)

const activeCount = computed(() => credentials.value.filter((k) => k.status === 'active').length)
const activeRequests = computed(() => credentials.value.reduce((n, k) => n + Number(k.active_requests || 0), 0))

function fmtTime(value) {
  if (!value) return '从未'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString('zh-CN', { hour12: false })
}

async function load() {
  loading.value = true
  const response = await api('/api-credentials?limit=500')
  if (response.ok) {
    credentials.value = listOf(response.data)
    error.value = ''
  } else error.value = response.error
  loading.value = false
}

function secretOf(data) {
  const body = data?.data || data || {}
  return body.secret || body.api_key || body.key || ''
}

function credentialOf(data) {
  return data?.data || data?.credential || data || {}
}

function showSecret(data, title) {
  const secret = secretOf(data)
  if (!secret) {
    error.value = '服务端未返回新 Key 明文，请勿继续使用该操作结果。'
    return false
  }
  revealed.value = { title, secret }
  copied.value = false
  return true
}

async function createCredential() {
  if (!form.name.trim()) return
  busy.value = 'create'
  const response = await api('/api-credentials', jsonBody('POST', {
    name: form.name.trim(),
    concurrency_limit: Math.max(0, Number(form.concurrency_limit) || 0),
  }))
  if (response.ok) {
    creating.value = false
    showSecret(response.data, '新 API Key')
    form.name = ''
    form.concurrency_limit = 0
    await load()
  } else error.value = response.error
  busy.value = ''
}

async function patchCredential(item, patch, field) {
  busy.value = `${item.id}:${field}`
  const response = await api(`/api-credentials/${encodeURIComponent(item.id)}`, jsonBody('PATCH', patch))
  if (response.ok) Object.assign(item, credentialOf(response.data), patch)
  else error.value = response.error
  busy.value = ''
}

async function toggleCredential(item) {
  await patchCredential(item, { status: item.status === 'active' ? 'disabled' : 'active' }, 'status')
}

async function rotateCredential(item) {
  if (!confirm(`轮换 ${item.name} 后，旧 Key 将立即失效。确认继续？`)) return
  busy.value = `${item.id}:rotate`
  const response = await api(`/api-credentials/${encodeURIComponent(item.id)}/rotate`, jsonBody('POST', {}))
  if (response.ok) {
    showSecret(response.data, `已轮换：${item.name}`)
    await load()
  } else error.value = response.error
  busy.value = ''
}

async function revokeCredential(item) {
  if (!confirm(`确认吊销 ${item.name}？该 Key 的历史日志仍会保留。`)) return
  busy.value = `${item.id}:revoke`
  const response = await api(`/api-credentials/${encodeURIComponent(item.id)}`, { method: 'DELETE' })
  if (response.ok) await load()
  else error.value = response.error
  busy.value = ''
}

async function copySecret() {
  copied.value = await copyText(revealed.value?.secret || '')
}

onMounted(load)
</script>

<template>
  <section class="space-y-4">
    <div class="flex items-start justify-between gap-4">
      <div>
        <h2 class="text-xl font-semibold text-white/90">服务 API Key</h2>
        <p class="mt-1 text-xs text-white/40">下游统一使用 OpenAI 格式 <code>Authorization: Bearer sk-...</code>。</p>
      </div>
      <button class="btn-primary" @click="creating = true"><Icon name="plus" class="w-3.5 h-3.5" />创建 Key</button>
    </div>

    <p v-if="error" class="notice">{{ error }}</p>

    <div class="grid grid-cols-2 md:grid-cols-3 gap-3 max-w-3xl">
      <div class="card p-4"><div class="kpi-label">总数</div><div class="kpi">{{ credentials.length }}</div></div>
      <div class="card p-4"><div class="kpi-label">有效</div><div class="kpi text-emerald-300">{{ activeCount }}</div></div>
      <div class="card p-4"><div class="kpi-label">当前请求</div><div class="kpi text-violet-300">{{ activeRequests }}</div></div>
    </div>

    <div class="card overflow-hidden">
      <div v-if="loading" class="py-16 text-center text-xs text-white/35">加载中…</div>
      <div v-else-if="!credentials.length" class="py-16 text-center text-xs text-white/35">还没有 API Key</div>
      <div v-else class="table-wrap">
        <table class="w-full text-xs min-w-[850px]">
          <thead><tr class="table-head"><th>名称</th><th>Key</th><th>并发</th><th>当前请求</th><th>最后使用</th><th>状态</th><th class="text-right">操作</th></tr></thead>
          <tbody>
            <tr v-for="item in credentials" :key="item.id" class="table-row">
              <td><div class="font-medium text-white/85">{{ item.name }}</div><div class="mt-1 text-[10px] text-white/30">{{ fmtTime(item.created_at) }}</div></td>
              <td><code class="font-mono text-[11px] text-white/60">{{ item.key_preview || 'sk-…' }}</code></td>
              <td><input type="number" min="0" class="field !py-1 !px-2 w-20 text-xs" :value="item.concurrency_limit || 0" title="0 = 不限" @change="patchCredential(item,{concurrency_limit:Math.max(0,Number($event.target.value)||0)},'concurrency')" /><span class="ml-2 text-[10px] text-white/30">{{ item.concurrency_limit ? '' : '不限' }}</span></td>
              <td class="tabular-nums text-white/60">{{ item.active_requests || 0 }}</td>
              <td class="text-white/45">{{ fmtTime(item.last_used_at) }}</td>
              <td><button class="switch" :class="item.status === 'active' && 'on'" :disabled="item.status === 'revoked'" @click="toggleCredential(item)"><span></span></button></td>
              <td><div class="flex justify-end gap-1"><button class="small-btn" :disabled="busy === `${item.id}:rotate` || item.status === 'revoked'" @click="rotateCredential(item)">轮换</button><button class="small-btn danger" :disabled="item.status === 'revoked'" @click="revokeCredential(item)">吊销</button></div></td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="creating" class="modal-bg" @click.self="creating = false">
      <form class="modal-card max-w-md" @submit.prevent="createCredential">
        <div class="flex justify-between items-center"><h3 class="font-semibold text-white/90">创建 API Key</h3><button type="button" @click="creating = false"><Icon name="close" class="w-4 h-4" /></button></div>
        <label><span class="label">名称</span><input v-model="form.name" class="field mt-1.5" placeholder="例如：production-worker" autofocus /></label>
        <label><span class="label">单 Key 并发限制</span><input v-model.number="form.concurrency_limit" type="number" min="0" class="field mt-1.5" /><span class="hint">0 = 不限；并发槽按 API Key 隔离。</span></label>
        <button class="btn-primary justify-center" :disabled="busy === 'create'">创建</button>
      </form>
    </div>

    <div v-if="revealed" class="modal-bg" @click.self="revealed = null">
      <div class="modal-card max-w-xl">
        <div class="flex justify-between items-center"><h3 class="font-semibold text-white/90">{{ revealed.title }}</h3><button @click="revealed = null"><Icon name="close" class="w-4 h-4" /></button></div>
        <div class="rounded-xl bg-amber-500/10 ring-1 ring-amber-400/25 px-3.5 py-3 text-xs text-amber-200">请立即复制并安全保存。关闭后不再显示明文。</div>
        <code class="block rounded-xl bg-black/35 ring-1 ring-white/10 p-4 break-all select-all text-sm text-emerald-300">{{ revealed.secret }}</code>
        <button class="btn-primary justify-center" @click="copySecret"><Icon name="copy" class="w-4 h-4" />{{ copied ? '已复制' : '复制 Key' }}</button>
      </div>
    </div>
  </section>
</template>

<style scoped>
.notice{border-radius:.7rem;padding:.7rem .9rem;font-size:.72rem;color:rgb(253 164 175);background:rgb(244 63 94 / .09);box-shadow:inset 0 0 0 1px rgb(244 63 94 / .22)}
.kpi-label{font-size:.65rem;text-transform:uppercase;letter-spacing:.08em;color:rgb(255 255 255 / .38)}.kpi{margin-top:.25rem;font-size:1.6rem;font-weight:650;line-height:1.2}
.table-wrap{overflow-x:auto}.table-head{border-bottom:1px solid rgb(255 255 255 / .06);font-size:.62rem;text-transform:uppercase;letter-spacing:.1em;color:rgb(255 255 255 / .35)}.table-head th,.table-row td{padding:.8rem 1rem;text-align:left}.table-head th:last-child{text-align:right}.table-row{border-bottom:1px solid rgb(255 255 255 / .045)}.table-row:last-child{border-bottom:0}.table-row:hover{background:rgb(255 255 255 / .025)}
.switch{position:relative;width:2.2rem;height:1.25rem;border-radius:999px;background:rgb(255 255 255 / .12)}.switch span{position:absolute;width:.95rem;height:.95rem;left:.15rem;top:.15rem;border-radius:999px;background:white;transition:transform .15s}.switch.on{background:rgb(16 185 129 / .7)}.switch.on span{transform:translateX(.95rem)}.switch:disabled{opacity:.35}
.small-btn{padding:.35rem .55rem;border-radius:.45rem;font-size:.65rem;color:rgb(255 255 255 / .58);background:rgb(255 255 255 / .04);box-shadow:inset 0 0 0 1px rgb(255 255 255 / .07)}.small-btn:hover{color:white;background:rgb(255 255 255 / .09)}.small-btn.danger{color:rgb(253 164 175)}
.modal-bg{position:fixed;inset:0;z-index:50;display:grid;place-items:center;padding:1rem;background:rgb(0 0 0 / .65);backdrop-filter:blur(6px)}.modal-card{width:100%;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .7);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .45),inset 0 0 0 1px rgb(255 255 255 / .08)}.label{display:block;font-size:.68rem;font-weight:600;color:rgb(255 255 255 / .48)}.hint{display:block;margin-top:.35rem;font-size:.62rem;color:rgb(255 255 255 / .3)}
</style>
