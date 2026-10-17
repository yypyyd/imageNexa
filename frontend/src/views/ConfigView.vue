<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import Icon from '../components/Icon.vue'
import { api, apiURL, jsonBody } from '../api'
import { ACCOUNT_PROVIDERS } from '../models'

const sections = [
  { id: 'general', label: '基础设置', description: '访问地址与图像处理', icon: 'config' },
  { id: 'network', label: '渠道与网络', description: '平台开关与上游出口', icon: 'plug' },
  { id: 'retention', label: '数据保留', description: '日志与成品清理策略', icon: 'log' },
]
const providerLabels = { adobe: 'Adobe Firefly', byteplus: 'BytePlus', chatgpt: 'ChatGPT', dola: 'Dola', grok: 'Grok' }
const providerOrder = ['chatgpt', 'grok', 'dola', 'adobe', 'byteplus'].filter((pool) => ACCOUNT_PROVIDERS.includes(pool))
const inheritsDefault = new Set(['chatgpt', 'grok', 'dola'])
const emptyProviderValues = () => Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, '']))
const activeSection = ref('general')
const expandedProvider = ref(null)
const settingsForm = ref(null)
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const saved = ref(false)
const snapshot = ref(null)
const submitted = ref(false)
const health = reactive({ live: null, ready: null })
const checkingHealth = ref(false)
const settings = reactive({
  public_base_url: '', outbound_proxy: '', outbound_extract_api: '', deai_enabled: false,
  logs_retention_days: 30, artifacts_retention_days: 30,
  provider_proxies: emptyProviderValues(), provider_extract_apis: emptyProviderValues(),
  providers_enabled: {},
})

const visibleProviders = computed(() => providerOrder.filter((pool) => typeof settings.providers_enabled[pool] === 'boolean'))
const enabledCount = computed(() => visibleProviders.value.filter((pool) => settings.providers_enabled[pool]).length)
const currentSection = computed(() => sections.find((section) => section.id === activeSection.value))
const payload = computed(() => ({
  public_base_url: settings.public_base_url.trim(),
  outbound_proxy: settings.outbound_proxy.trim(),
  outbound_extract_api: settings.outbound_extract_api.trim(),
  provider_extract_apis: Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, settings.provider_extract_apis[pool].trim()])),
  provider_proxies: Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, settings.provider_proxies[pool].trim()])),
  providers_enabled: { ...settings.providers_enabled },
  deai_enabled: settings.deai_enabled,
  logs_retention_days: Number(settings.logs_retention_days),
  artifacts_retention_days: Number(settings.artifacts_retention_days),
}))

function sectionFor(field) {
  if (field.endsWith('_retention_days')) return 'retention'
  return ['public_base_url', 'deai_enabled'].includes(field) ? 'general' : 'network'
}

const changes = computed(() => {
  if (!snapshot.value) return []
  return Object.entries(payload.value).flatMap(([field, value]) => {
    if (value && typeof value === 'object') {
      return Object.entries(value).filter(([key, item]) => item !== snapshot.value[field]?.[key]).map(() => sectionFor(field))
    }
    return value !== snapshot.value[field] ? [sectionFor(field)] : []
  })
})
const dirty = computed(() => changes.value.length > 0)
const sectionChanges = (id) => changes.value.filter((section) => section === id).length

async function selectSection(id) {
  activeSection.value = id
  await nextTick()
  settingsForm.value?.scrollIntoView({ block: 'start' })
}

function applySettings(data) {
  for (const field of ['public_base_url', 'outbound_proxy', 'outbound_extract_api']) settings[field] = String(data[field] || '')
  for (const field of ['provider_proxies', 'provider_extract_apis']) {
    settings[field] = Object.fromEntries(ACCOUNT_PROVIDERS.map((pool) => [pool, String(data[field]?.[pool] || '')]))
  }
  if (!settings.provider_extract_apis.dola && data.dola_session_api) settings.provider_extract_apis.dola = String(data.dola_session_api)
  settings.providers_enabled = Object.fromEntries(ACCOUNT_PROVIDERS
    .filter((pool) => typeof data.providers_enabled?.[pool] === 'boolean')
    .map((pool) => [pool, data.providers_enabled[pool]]))
  settings.deai_enabled = data.deai_enabled === true
  settings.logs_retention_days = data.logs_retention_days ?? 30
  settings.artifacts_retention_days = data.artifacts_retention_days ?? 30
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const response = await api('/settings')
    if (!response.ok) throw new Error(response.error)
    applySettings(response.data?.data || response.data || {})
    snapshot.value = structuredClone(payload.value)
  } catch (cause) {
    error.value = cause.message || '无法加载设置，请稍后重试。'
  } finally {
    loading.value = false
  }
}

const validationErrors = computed(() => {
  const errors = {}
  function validateURL(field, value, kind, pool = '') {
    if (!value) return
    let valid = false
    try {
      const url = new URL(value)
      valid = /^[a-z][a-z0-9+.-]*:\/\//i.test(value) && !!url.hostname && (kind === 'proxy'
        ? ['http:', 'https:', 'socks5:', 'socks5h:'].includes(url.protocol) && !url.search && !url.hash
        : ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password)
      if (kind === 'origin') valid = valid && (!url.pathname || url.pathname === '/') && !url.search && !url.hash
    } catch { /* Validate without rewriting the operator's URL. */ }
    if (!valid) errors[field] = {
      section: sectionFor(field), pool,
      message: kind === 'origin' ? '请输入完整的 HTTP(S) 域名，不含 /v1、路径、查询参数或账号信息。'
        : kind === 'proxy' ? '请输入 HTTP(S) 或 SOCKS5 代理地址，不含查询参数或片段。'
          : '请输入完整的 HTTP(S) 提取地址，账号信息请放在查询参数中。',
    }
  }
  validateURL('public_base_url', payload.value.public_base_url, 'origin')
  validateURL('outbound_extract_api', payload.value.outbound_extract_api, 'extract')
  validateURL('outbound_proxy', payload.value.outbound_proxy, 'proxy')
  for (const pool of ACCOUNT_PROVIDERS) {
    validateURL(`provider_extract_apis-${pool}`, payload.value.provider_extract_apis[pool], 'extract', pool)
    validateURL(`provider_proxies-${pool}`, payload.value.provider_proxies[pool], 'proxy', pool)
  }
  for (const field of ['logs_retention_days', 'artifacts_retention_days']) {
    const value = payload.value[field]
    if (!Number.isInteger(value) || value < 1 || value > 3650) errors[field] = { section: 'retention', message: '请输入 1–3650 之间的整数天数。' }
  }
  return errors
})
const fieldError = (field) => submitted.value ? validationErrors.value[field]?.message : ''

async function save() {
  if (loading.value || saving.value || !snapshot.value || !dirty.value) return
  submitted.value = true
  saved.value = false
  error.value = ''
  const [field, invalid] = Object.entries(validationErrors.value)[0] || []
  if (invalid) {
    activeSection.value = invalid.section
    if (invalid.pool) expandedProvider.value = invalid.pool
    await nextTick()
    document.getElementById(`setting-${field}`)?.focus()
    return
  }
  saving.value = true
  try {
    const response = await api('/settings', jsonBody('PUT', payload.value))
    if (!response.ok) throw new Error(response.error)
    applySettings(response.data?.data || response.data || {})
    snapshot.value = structuredClone(payload.value)
    submitted.value = false
    saved.value = true
  } catch (cause) {
    error.value = cause.message || '保存失败，更改已保留，请重试。'
  } finally {
    saving.value = false
  }
}

function discard() {
  applySettings(snapshot.value)
  submitted.value = false
  error.value = ''
  saved.value = false
}

function routeSummary(pool) {
  if (settings.provider_extract_apis[pool].trim() || settings.provider_proxies[pool].trim()) return '单独配置'
  if (inheritsDefault.has(pool) && (settings.outbound_extract_api.trim() || settings.outbound_proxy.trim())) return '继承默认出口'
  return '直连'
}

function effectiveRoute(pool) {
  const inherit = inheritsDefault.has(pool)
  const extract = settings.provider_extract_apis[pool].trim() || (inherit && settings.outbound_extract_api.trim())
  const proxy = settings.provider_proxies[pool].trim() || (inherit && settings.outbound_proxy.trim())
  if (extract) return proxy ? '优先提取代理，提取失败时使用固定代理。' : '优先使用提取 API 分配的代理。'
  return proxy ? '通过固定代理连接上游。' : '直接连接上游。'
}

async function checkHealth() {
  if (checkingHealth.value) return
  checkingHealth.value = true
  await Promise.all(['live', 'ready'].map(async (name) => {
    health[name] = null
    try {
      const response = await fetch(apiURL(`/health/${name}`), { headers: { Accept: 'application/json' }, signal: AbortSignal.timeout(10000) })
      health[name] = response.ok
    } catch { health[name] = false }
  }))
  checkingHealth.value = false
}

function beforeUnload(event) {
  if (dirty.value) { event.preventDefault(); event.returnValue = '' }
}

watch(payload, () => { if (dirty.value) saved.value = false })
onBeforeRouteLeave(() => !dirty.value || window.confirm('还有未保存的设置，确定离开并放弃这些更改吗？'))
onMounted(() => { load(); checkHealth(); window.addEventListener('beforeunload', beforeUnload) })
onBeforeUnmount(() => window.removeEventListener('beforeunload', beforeUnload))
</script>

<template>
  <section class="settings-page">
    <div class="page-head">
      <p class="kicker">SETTINGS / 07</p>
      <h2 class="page-title">系统设置</h2>
      <p class="page-sub">统一管理访问地址、渠道出口与数据保留策略。</p>
    </div>

    <form ref="settingsForm" class="settings-shell" novalidate @submit.prevent="save">
      <aside class="settings-sidebar">
        <div class="sidebar-content">
          <p class="sidebar-caption">配置目录</p>
          <nav class="settings-nav" aria-label="设置分区">
            <button v-for="section in sections" :key="section.id" type="button" :class="['section-link', { active: activeSection === section.id }]" :aria-current="activeSection === section.id ? 'true' : undefined" :aria-controls="`panel-${section.id}`" @click="selectSection(section.id)">
              <Icon :name="section.icon" class="nav-icon" aria-hidden="true" />
              <span class="nav-text"><strong>{{ section.label }}</strong><small>{{ section.description }}</small></span>
              <span v-if="sectionChanges(section.id)" class="change-dot" :aria-label="`${sectionChanges(section.id)} 项未保存`"></span>
            </button>
          </nav>
          <div class="sidebar-health">
            <div class="health-heading"><span>运行状态</span><button type="button" class="refresh-health" aria-label="刷新运行状态" :disabled="checkingHealth" @click="checkHealth"><Icon name="refresh" :class="{ spinning: checkingHealth }" /></button></div>
            <div v-for="(label, name) in { live: '服务存活', ready: '依赖就绪' }" :key="name" class="health-line">
              <span>{{ label }}</span><span :class="['health-value', health[name] === null ? 'waiting' : health[name] ? 'healthy' : 'unhealthy']"><i></i>{{ health[name] === null ? '检查中' : health[name] ? '正常' : '异常' }}</span>
            </div>
          </div>
          <p class="sidebar-note"><Icon name="info" />所有分区的更改会统一保存，切换分区不会丢失编辑内容。</p>
        </div>
      </aside>

      <div class="settings-main">
        <header class="panel-heading">
          <div><h3>{{ currentSection.label }}</h3><p>{{ currentSection.description }}</p></div>
          <span class="section-index">0{{ sections.findIndex((section) => section.id === activeSection) + 1 }} / 03</span>
        </header>

        <div v-if="error" class="settings-alert" role="alert"><Icon name="info" /><span>{{ error }}</span><button v-if="!snapshot" type="button" @click="load">重新加载</button></div>
        <div v-if="submitted && Object.keys(validationErrors).length" class="settings-alert" role="alert"><Icon name="info" /><span>请修正标注的设置项后再保存。</span></div>
        <div v-if="loading" class="loading-state" role="status"><Icon name="refresh" class="spinning" />正在加载设置…</div>
        <div v-else-if="!snapshot" class="empty-state"><Icon name="config" /><h4>暂时无法读取设置</h4><p>重新加载后即可继续配置。</p><button type="button" class="btn-soft" @click="load">重试</button></div>

        <fieldset v-else class="settings-fields" :disabled="saving">
          <section v-show="activeSection === 'general'" id="panel-general" aria-label="基础设置">
            <div class="setting-group">
              <div class="group-heading"><h4>公开 API</h4><span>对外访问</span></div>
              <div class="setting-row">
                <div class="setting-label"><label for="setting-public_base_url">对外 Base URL</label><p>用于 API 文档示例和生成成品的访问链接。</p></div>
                <div class="setting-control">
                  <input id="setting-public_base_url" v-model="settings.public_base_url" class="field mono-field" placeholder="https://api.example.com" autocomplete="off" spellcheck="false" :aria-invalid="!!fieldError('public_base_url')" aria-describedby="base-url-hint base-url-error" />
                  <p id="base-url-hint" class="field-hint">填写域名即可，不含 <code>/v1</code> 或其他路径；生产环境请使用 HTTPS。</p>
                  <p v-if="fieldError('public_base_url')" id="base-url-error" class="field-error">{{ fieldError('public_base_url') }}</p>
                </div>
              </div>
            </div>
            <div class="setting-group">
              <div class="group-heading"><h4>图像处理</h4><span>请求行为</span></div>
              <div class="setting-row toggle-row">
                <div class="setting-label"><label id="deai-label" for="setting-deai_enabled">去 AI 特征</label><p id="deai-hint">允许图像请求通过 <code>deai</code> 参数启用处理。关闭后会忽略该参数。</p></div>
                <div class="toggle-control"><span>{{ settings.deai_enabled ? '已启用' : '未启用' }}</span><button id="setting-deai_enabled" type="button" role="switch" :aria-checked="settings.deai_enabled" aria-labelledby="deai-label" aria-describedby="deai-hint" :class="['setting-switch', { on: settings.deai_enabled }]" @click="settings.deai_enabled = !settings.deai_enabled"><span></span></button></div>
              </div>
            </div>
            <div class="panel-note"><Icon name="info" /><p>平台启用状态和各平台的代理地址，统一在<button type="button" @click="selectSection('network')">渠道与网络<Icon name="open" /></button>中管理。</p></div>
          </section>

          <section v-show="activeSection === 'network'" id="panel-network" aria-label="渠道与网络">
            <div class="setting-group">
              <div class="group-heading"><h4>默认出口</h4><span>ChatGPT · Grok · Dola</span></div>
              <p class="group-description">三个平台默认共用以下出口。Adobe 和 BytePlus 默认直连，可在平台中单独指定。</p>
              <div class="setting-row">
                <div class="setting-label"><label for="setting-outbound_extract_api">代理提取 API</label><p>优先从提取接口获取代理，按账号分配出口。</p></div>
                <div class="setting-control">
                  <input id="setting-outbound_extract_api" v-model="settings.outbound_extract_api" class="field mono-field" placeholder="https://proxy.example.com/extract?count=64" autocomplete="off" spellcheck="false" :aria-invalid="!!fieldError('outbound_extract_api')" aria-describedby="extract-hint extract-error" />
                  <p id="extract-hint" class="field-hint">可选。每次最多提取 64 条代理，留空则跳过提取。</p>
                  <p v-if="fieldError('outbound_extract_api')" id="extract-error" class="field-error">{{ fieldError('outbound_extract_api') }}</p>
                </div>
              </div>
              <div class="setting-row">
                <div class="setting-label"><label for="setting-outbound_proxy">固定代理</label><p>未配置提取 API 或提取失败时，使用此代理。</p></div>
                <div class="setting-control">
                  <input id="setting-outbound_proxy" v-model="settings.outbound_proxy" class="field mono-field" placeholder="http://user:pass@host:port" autocomplete="off" spellcheck="false" :aria-invalid="!!fieldError('outbound_proxy')" aria-describedby="proxy-hint proxy-error" />
                  <p id="proxy-hint" class="field-hint">支持 HTTP(S) / SOCKS5。两项都留空时直连。</p>
                  <p v-if="fieldError('outbound_proxy')" id="proxy-error" class="field-error">{{ fieldError('outbound_proxy') }}</p>
                </div>
              </div>
            </div>

            <div class="setting-group provider-group">
              <div class="group-heading"><h4>平台与出口</h4><span>{{ enabledCount }} / {{ visibleProviders.length }} 个已启用</span></div>
              <p class="group-description">在同一处管理平台开关与独立出口。停用后停止从该号池选号，已有配置会保留。</p>
              <div v-for="pool in visibleProviders" :key="pool" :class="['channel', { expanded: expandedProvider === pool }]">
                <div class="channel-row">
                  <span :class="['provider-mark', `mark-${pool}`]" aria-hidden="true">{{ pool === 'adobe' ? 'Af' : pool === 'byteplus' ? 'B+' : pool === 'chatgpt' ? 'C' : pool === 'grok' ? 'G' : 'D' }}</span>
                  <div class="channel-name"><strong>{{ providerLabels[pool] }}</strong><span>{{ pool === 'dola' ? '豆包国际版' : pool }}</span></div>
                  <span :class="['route-badge', { custom: routeSummary(pool) === '单独配置' }]">{{ routeSummary(pool) }}</span>
                  <div class="channel-actions">
                    <button type="button" role="switch" :aria-label="`启用 ${providerLabels[pool]}`" :aria-checked="settings.providers_enabled[pool]" :class="['setting-switch', { on: settings.providers_enabled[pool] }]" @click="settings.providers_enabled[pool] = !settings.providers_enabled[pool]"><span></span></button>
                    <button type="button" class="configure-channel" :aria-label="`${providerLabels[pool]} 出口配置`" :aria-expanded="expandedProvider === pool" :aria-controls="`channel-${pool}`" @click="expandedProvider = expandedProvider === pool ? null : pool">配置<Icon name="chevron" /></button>
                  </div>
                </div>
                <div v-show="expandedProvider === pool" :id="`channel-${pool}`" class="channel-detail">
                  <p class="inherit-note"><Icon name="info" />{{ inheritsDefault.has(pool) ? '单项留空时继承对应的默认出口配置。' : '留空时直连，不继承默认出口配置。' }}</p>
                  <div class="channel-inputs">
                    <div>
                      <label :for="`setting-provider_extract_apis-${pool}`">代理提取 API</label>
                      <input :id="`setting-provider_extract_apis-${pool}`" v-model="settings.provider_extract_apis[pool]" class="field mono-field" :placeholder="inheritsDefault.has(pool) ? '留空继承默认提取 API' : '留空不使用提取 API'" autocomplete="off" spellcheck="false" :aria-invalid="!!fieldError(`provider_extract_apis-${pool}`)" :aria-describedby="`extract-error-${pool}`" />
                      <p v-if="fieldError(`provider_extract_apis-${pool}`)" :id="`extract-error-${pool}`" class="field-error">{{ fieldError(`provider_extract_apis-${pool}`) }}</p>
                    </div>
                    <div>
                      <label :for="`setting-provider_proxies-${pool}`">固定代理</label>
                      <input :id="`setting-provider_proxies-${pool}`" v-model="settings.provider_proxies[pool]" class="field mono-field" :placeholder="inheritsDefault.has(pool) ? '留空继承默认固定代理' : '留空时直连'" autocomplete="off" spellcheck="false" :aria-invalid="!!fieldError(`provider_proxies-${pool}`)" :aria-describedby="`proxy-error-${pool}`" />
                      <p v-if="fieldError(`provider_proxies-${pool}`)" :id="`proxy-error-${pool}`" class="field-error">{{ fieldError(`provider_proxies-${pool}`) }}</p>
                    </div>
                  </div>
                  <p class="effective-route"><span>当前策略</span>{{ effectiveRoute(pool) }}<em v-if="!settings.providers_enabled[pool]">平台已停用，保存后不参与调度。</em></p>
                </div>
              </div>
              <p v-if="!visibleProviders.length" class="group-description">未获取到可配置的平台。</p>
            </div>
          </section>

          <section v-show="activeSection === 'retention'" id="panel-retention" aria-label="数据保留">
            <div class="setting-group">
              <div class="group-heading"><h4>自动清理</h4><span>按天执行</span></div>
              <p class="group-description">设置日志和成品的保留时长，超过期限后由系统自动清理。</p>
              <div v-for="item in [{ field: 'logs_retention_days', label: '调用日志', description: '保留请求记录、调用结果与错误信息。' }, { field: 'artifacts_retention_days', label: '生成成品', description: '保留生成的图片、视频及其访问链接。' }]" :key="item.field" class="setting-row">
                <div class="setting-label"><label :for="`setting-${item.field}`">{{ item.label }}</label><p>{{ item.description }}</p></div>
                <div class="setting-control">
                  <div class="days-input"><input :id="`setting-${item.field}`" v-model="settings[item.field]" type="number" min="1" max="3650" step="1" class="field" :aria-invalid="!!fieldError(item.field)" :aria-describedby="`${item.field}-hint ${item.field}-error`" /><span aria-hidden="true">天</span></div>
                  <p :id="`${item.field}-hint`" class="field-hint">可设置 1–3650 天，默认 30 天。</p>
                  <p v-if="fieldError(item.field)" :id="`${item.field}-error`" class="field-error">{{ fieldError(item.field) }}</p>
                </div>
              </div>
            </div>
            <div class="retention-warning"><Icon name="info" /><div><strong>缩短保留时间会影响已有数据</strong><p>保存后，超出新期限的日志和成品会在后续清理时删除，过期成品链接将不可访问。API 密钥归属信息会保留。</p></div></div>
          </section>
        </fieldset>
      </div>

      <footer class="save-bar">
        <div class="save-status" role="status" aria-live="polite">
          <span :class="['save-status-icon', { pending: dirty, success: saved }]"><Icon :name="dirty ? 'info' : 'check'" /></span>
          <div><strong>{{ saving ? '正在保存…' : dirty ? `${changes.length} 项更改未保存` : saved ? '设置已保存' : snapshot ? '所有设置已同步' : '等待加载设置' }}</strong><p>{{ dirty ? '保存后，所有分区的更改将统一生效。' : '修改配置后，在此统一保存。' }}</p></div>
        </div>
        <div class="save-actions"><button type="button" class="discard-button" :disabled="!dirty || saving" @click="discard">放弃更改</button><button type="submit" class="btn-primary save-button" :disabled="loading || saving || !dirty || !snapshot"><Icon :name="saving ? 'refresh' : 'check'" :class="{ spinning: saving }" />{{ saving ? '保存中…' : '保存更改' }}</button></div>
      </footer>
    </form>
  </section>
</template>

<style scoped>
.settings-page { display: grid; gap: 24px; max-width: 1280px; margin: 0 auto; }
.settings-shell { display: grid; grid-template-columns: 220px minmax(0, 1fr); align-items: start; background: var(--surface); border: 1px solid var(--hairline); border-radius: 16px; box-shadow: var(--shadow-card); }
.settings-sidebar { padding: 26px 16px; align-self: stretch; border-right: 1px solid var(--hairline); background: color-mix(in srgb, var(--surface-2) 38%, transparent); border-radius: 16px 0 0 0; }
.sidebar-content { position: sticky; top: 24px; }
.sidebar-caption { margin: 0 12px 14px; color: var(--fg-3); font-size: 10px; font-weight: 600; letter-spacing: .12em; }
.settings-nav { display: grid; gap: 6px; }
.section-link { display: flex; align-items: center; gap: 11px; width: 100%; padding: 13px 12px; border: 1px solid transparent; border-radius: 10px; text-align: left; transition: background .15s, color .15s; color: var(--fg-2); }
.section-link:hover { background: var(--hover); }
.section-link.active { color: var(--accent-ink); background: var(--accent-soft); border-color: color-mix(in srgb, var(--accent) 14%, transparent); }
.nav-icon { width: 18px; height: 18px; flex: none; }
.nav-text { display: grid; gap: 4px; }
.nav-text strong { font-size: 13px; font-weight: 600; }
.nav-text small { font-size: 10px; color: var(--fg-3); }
.change-dot { width: 6px; height: 6px; flex: none; margin-left: auto; border-radius: 50%; background: var(--warn); }
.sidebar-health { border-top: 1px solid var(--hairline); margin: 34px 12px 0; padding-top: 18px; }
.health-heading { display: flex; justify-content: space-between; align-items: center; margin-bottom: 10px; color: var(--fg-3); font-size: 11px; }
.refresh-health { display: grid; place-items: center; width: 26px; height: 26px; border-radius: 6px; }
.refresh-health:hover { background: var(--hover); color: var(--fg); }
.refresh-health svg { width: 13px; height: 13px; }
.health-line { display: flex; justify-content: space-between; align-items: center; margin-top: 10px; font-size: 11px; color: var(--fg-2); }
.health-value { display: flex; align-items: center; gap: 6px; font-size: 10px; }
.health-value i { width: 5px; height: 5px; border-radius: 50%; background: currentColor; }
.healthy { color: var(--ok); }.unhealthy { color: var(--bad); }.waiting { color: var(--fg-3); }
.sidebar-note { display: flex; gap: 7px; margin: 25px 12px 0; font-size: 10px; line-height: 1.8; color: var(--fg-3); }
.sidebar-note svg { width: 13px; height: 13px; flex: none; margin-top: 3px; }
.settings-main { min-width: 0; min-height: 480px; padding: 28px 32px 34px; }
.panel-heading { display: flex; align-items: center; justify-content: space-between; padding-bottom: 23px; margin-bottom: 4px; border-bottom: 1px solid var(--hairline); }
.panel-heading h3 { color: var(--fg); font-size: 17px; font-weight: 650; }
.panel-heading p { font-size: 11px; color: var(--fg-3); margin-top: 5px; }
.section-index { font-family: var(--font-mono); color: var(--fg-faint); font-size: 10px; letter-spacing: .05em; }
.settings-fields { min-width: 0; border: 0; padding: 0; margin: 0; }
.settings-fields:disabled { opacity: .7; }
.setting-group { padding: 26px 0 4px; }
.setting-group + .setting-group { border-top: 1px solid var(--hairline); margin-top: 14px; }
.group-heading { display: flex; align-items: center; gap: 14px; justify-content: space-between; }
.group-heading h4 { color: var(--fg); font-size: 13px; font-weight: 600; }
.group-heading > span { font-size: 10px; color: var(--fg-3); }
.group-description { margin: 9px 0 4px; color: var(--fg-3); font-size: 11px; line-height: 1.8; }
.setting-row { display: grid; grid-template-columns: minmax(145px, .75fr) minmax(0, 1.4fr); gap: 28px; padding: 24px 0; }
.setting-row + .setting-row { border-top: 1px solid var(--hairline-soft); }
.setting-label label { display: block; margin-top: 9px; color: var(--fg); font-size: 12px; font-weight: 500; }
.setting-label p { font-size: 11px; line-height: 1.8; color: var(--fg-3); margin-top: 7px; max-width: 260px; }
.setting-label code, .field-hint code { font-family: var(--font-mono); color: var(--fg-2); }
.setting-control { min-width: 0; }
.settings-fields .field { padding: 11px 12px; font-size: 12px; border-radius: 8px; }
.settings-fields .mono-field { font-family: var(--font-mono); font-size: 11px; }
.field-hint { margin: 8px 0 0; color: var(--fg-3); font-size: 10px; line-height: 1.8; }
.field-error { margin-top: 7px; color: var(--bad); font-size: 11px; line-height: 1.7; }
.settings-fields .field[aria-invalid="true"] { border-color: var(--bad); }
.toggle-row { grid-template-columns: minmax(0, 1fr) auto; align-items: center; }
.toggle-row .setting-label label { margin-top: 0; }
.toggle-row .setting-label p { max-width: 430px; }
.toggle-control { display: flex; align-items: center; gap: 12px; font-size: 11px; color: var(--fg-3); }
.setting-switch { position: relative; width: 34px; height: 20px; flex: none; border-radius: 999px; background: color-mix(in srgb, var(--fg) 18%, transparent); transition: background .15s; }
.setting-switch span { position: absolute; width: 14px; height: 14px; left: 3px; top: 3px; border-radius: 50%; background: #fff; box-shadow: 0 1px 3px rgb(0 0 0 / .15); transition: transform .15s; }
.setting-switch.on { background: var(--accent); }.setting-switch.on span { transform: translateX(14px); }
.settings-shell button:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }
.panel-note { display: flex; align-items: flex-start; gap: 9px; padding: 14px 16px; margin-top: 20px; border-radius: 8px; background: var(--surface-2); font-size: 11px; line-height: 1.8; color: var(--fg-3); }
.panel-note > svg { flex: none; width: 14px; height: 14px; margin-top: 3px; }
.panel-note button { display: inline-flex; align-items: center; gap: 3px; margin: 0 4px; color: var(--accent-ink); font-weight: 500; }
.panel-note button svg { width: 11px; height: 11px; }
.provider-group .group-description { margin-bottom: 20px; }
.channel { border: 1px solid var(--hairline); border-radius: 10px; margin-top: 9px; overflow: hidden; }
.channel.expanded { border-color: color-mix(in srgb, var(--accent) 35%, var(--hairline)); }
.channel-row { display: flex; align-items: center; gap: 12px; padding: 15px 16px; }
.provider-mark { display: grid; place-items: center; flex: none; width: 33px; height: 33px; border-radius: 9px; font-family: var(--font-mono); font-size: 12px; font-weight: 650; background: var(--surface-2); color: var(--fg-2); }
.mark-chatgpt { color: var(--ok); background: color-mix(in srgb, var(--ok) 9%, transparent); }
.mark-adobe { color: var(--bad); background: color-mix(in srgb, var(--bad) 8%, transparent); }
.mark-byteplus { color: var(--accent-ink); background: var(--accent-soft); }
.mark-dola { color: var(--warn); background: color-mix(in srgb, var(--warn) 9%, transparent); }
.channel-name { display: grid; gap: 3px; flex: 1; min-width: 0; }.channel-name strong { font-size: 12px; font-weight: 600; color: var(--fg); }.channel-name > span { font-family: var(--font-mono); font-size: 10px; color: var(--fg-3); }
.route-badge { font-size: 10px; color: var(--fg-3); border-radius: 5px; padding: 4px 7px; background: var(--surface-2); white-space: nowrap; }
.route-badge.custom { color: var(--accent-ink); background: var(--accent-soft); }
.channel-actions { display: flex; align-items: center; gap: 22px; margin-left: 8px; }
.configure-channel { display: flex; gap: 6px; align-items: center; padding: 6px 0 6px 6px; color: var(--fg-2); font-size: 11px; }.configure-channel:hover { color: var(--accent-ink); }.configure-channel svg { width: 13px; height: 13px; transition: transform .15s; }.expanded .configure-channel svg { transform: rotate(180deg); }
.channel-detail { padding: 0 16px 16px; border-top: 1px solid var(--hairline-soft); background: color-mix(in srgb, var(--surface-2) 35%, transparent); }
.inherit-note { display: flex; align-items: center; gap: 6px; padding: 13px 0; color: var(--fg-3); font-size: 10px; line-height: 1.7; }.inherit-note svg { width: 12px; height: 12px; flex: none; }
.channel-inputs { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }.channel-inputs label { display: block; font-size: 11px; font-weight: 500; color: var(--fg-2); margin-bottom: 7px; }
.effective-route { color: var(--fg-2); font-size: 10px; line-height: 1.9; margin-top: 13px; }.effective-route > span { color: var(--fg-3); margin-right: 10px; }.effective-route em { display: block; color: var(--warn); font-style: normal; margin-top: 4px; }
.days-input { position: relative; max-width: 180px; }.days-input .field { padding-right: 40px; font-family: var(--font-mono); }.days-input > span { position: absolute; right: 14px; top: 11px; color: var(--fg-3); font-size: 12px; }
.retention-warning { display: flex; gap: 10px; padding: 16px; margin-top: 18px; border-radius: 9px; border: 1px solid color-mix(in srgb, var(--warn) 18%, transparent); background: color-mix(in srgb, var(--warn) 5%, transparent); color: var(--fg-2); }
.retention-warning > svg { color: var(--warn); width: 15px; height: 15px; flex: none; margin-top: 2px; }.retention-warning strong { font-size: 11px; font-weight: 600; }.retention-warning p { font-size: 11px; color: var(--fg-3); line-height: 1.9; margin-top: 6px; }
.save-bar { grid-column: 1 / -1; display: flex; align-items: center; justify-content: space-between; gap: 16px; position: sticky; bottom: 0; z-index: 5; padding: 18px 26px; border-top: 1px solid var(--hairline); border-radius: 0 0 16px 16px; background: var(--menu-bg); box-shadow: 0 -6px 18px -12px rgb(0 0 0 / .15); }
.save-status { display: flex; align-items: center; gap: 10px; min-width: 0; }.save-status-icon { display: grid; place-items: center; width: 27px; height: 27px; border-radius: 50%; background: var(--surface-2); color: var(--fg-3); flex: none; }.save-status-icon svg { width: 13px; height: 13px; }.save-status-icon.pending { color: var(--warn); background: color-mix(in srgb, var(--warn) 10%, transparent); }.save-status-icon.success { color: var(--ok); background: color-mix(in srgb, var(--ok) 10%, transparent); }.save-status strong { color: var(--fg-2); font-size: 11px; font-weight: 500; }.save-status p { color: var(--fg-3); font-size: 10px; margin-top: 3px; }
.save-actions { display: flex; align-items: center; gap: 18px; flex: none; }.discard-button { font-size: 11px; color: var(--fg-2); }.discard-button:disabled { opacity: .35; }.discard-button:hover:not(:disabled) { color: var(--fg); }.save-button { font-size: 12px; padding: 10px 17px; }.save-button svg { width: 14px; height: 14px; }
.settings-alert { display: flex; align-items: center; gap: 9px; background: color-mix(in srgb, var(--bad) 6%, transparent); color: var(--bad); padding: 12px 14px; border-radius: 8px; font-size: 11px; line-height: 1.8; margin-top: 16px; }.settings-alert > svg { width: 15px; height: 15px; flex: none; }.settings-alert > span { flex: 1; overflow-wrap: anywhere; }.settings-alert button { font-weight: 600; text-decoration: underline; }
.loading-state { display: flex; align-items: center; justify-content: center; gap: 10px; padding: 120px 0; font-size: 12px; color: var(--fg-3); }.loading-state svg { width: 16px; height: 16px; }.empty-state { display: grid; justify-items: center; gap: 12px; padding: 70px 16px; color: var(--fg-3); font-size: 12px; }.empty-state > svg { width: 28px; height: 28px; }.empty-state h4 { color: var(--fg); font-size: 14px; }
.spinning { animation: spin 1s linear infinite; }@keyframes spin { to { transform: rotate(360deg); } }
@media (max-width: 1180px) {
  .settings-shell { grid-template-columns: 190px minmax(0, 1fr); }.settings-sidebar { padding: 24px 10px; }.settings-main { padding: 26px 24px; }.setting-row { grid-template-columns: minmax(130px, .7fr) minmax(0, 1.4fr); gap: 20px; }.channel-actions { gap: 14px; }.channel-inputs { grid-template-columns: 1fr; }
}
@media (max-width: 1020px) {
  .settings-sidebar { position: sticky; top: 0; z-index: 6; background: var(--menu-bg); align-self: start; }.sidebar-content { position: static; }
  .settings-shell { grid-template-columns: minmax(0, 1fr); }.settings-sidebar { border-right: 0; border-bottom: 1px solid var(--hairline); border-radius: 16px 16px 0 0; padding: 12px; }.sidebar-caption, .sidebar-note { display: none; }.settings-nav { display: flex; gap: 6px; }.section-link { justify-content: center; flex: 1; padding: 12px 8px; gap: 8px; }.nav-text small { display: none; }.nav-text strong { font-size: 12px; }.nav-icon { width: 16px; height: 16px; }.change-dot { margin-left: 0; }.settings-main { min-height: 420px; }.save-bar { padding: 16px 22px; }.sidebar-health { display: flex; align-items: center; justify-content: flex-end; gap: 16px; margin: 10px 4px 0; padding-top: 8px; }.health-heading { margin: 0 auto 0 0; }.health-heading > span { display: none; }.health-line { margin-top: 0; gap: 8px; font-size: 10px; }
}
@media (max-width: 600px) {
  .settings-page { gap: 18px; }.settings-sidebar { padding: 8px; }.section-link { padding: 12px 5px; gap: 6px; }.nav-icon { display: none; }.settings-main { padding: 22px 18px 26px; }.panel-heading { padding-bottom: 19px; }.panel-heading h3 { font-size: 16px; }.setting-row { grid-template-columns: 1fr; gap: 14px; padding: 20px 0; }.setting-label label { margin-top: 0; }.setting-label p { max-width: none; margin-top: 5px; }.toggle-row { grid-template-columns: minmax(0, 1fr) auto; gap: 18px; }.toggle-control > span { display: none; }.group-heading { gap: 8px; }.group-heading > span { font-size: 9px; }.channel-row { flex-wrap: wrap; padding: 13px 12px; gap: 10px; }.provider-mark { width: 29px; height: 29px; font-size: 11px; border-radius: 7px; }.channel-name strong { font-size: 11px; }.channel-name > span { font-size: 9px; }.route-badge { order: 4; flex-basis: 100%; background: transparent; padding: 0 0 0 39px; font-size: 9px; }.route-badge.custom { background: transparent; }.channel-actions { gap: 15px; margin-left: 0; }.channel-detail { padding: 0 12px 14px; }.save-bar { flex-wrap: wrap; gap: 14px; padding: 15px 18px; }.save-status p { display: none; }.save-status { gap: 7px; }.save-status strong { font-size: 10px; }.save-status-icon { width: 22px; height: 22px; }.save-actions { gap: 12px; margin-left: auto; }.discard-button { font-size: 10px; }.save-button { font-size: 11px; padding: 9px 12px; }.retention-warning { padding: 13px; }
}
@media (prefers-reduced-motion: reduce) { .spinning { animation: none; }.setting-switch, .setting-switch span, .configure-channel svg { transition: none; } }
</style>
