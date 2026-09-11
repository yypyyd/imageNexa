<script setup>
import { computed, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { IMAGE_MODELS, TEXT_MODELS, VIDEO_MODELS } from '../models'
import { copyText } from '../utils/clipboard'

const base = computed(() => `${location.protocol}//${location.host}`)
const copied = ref('')

const examples = computed(() => ({
  models: `curl ${base.value}/v1/models \\\n  -H "Authorization: Bearer sk-your-api-key"`,
  chat: `curl ${base.value}/v1/chat/completions \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  -H "Content-Type: application/json" \\\n  -d '{
    "model": "gpt-5-5-mini",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": false
  }'`,
  image: `curl ${base.value}/v1/images/generations \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  -H "Content-Type: application/json" \\\n  -H "Idempotency-Key: image-order-001" \\\n  -d '{
    "model": "gpt-image-2",
    "prompt": "A minimal product photograph",
    "n": 1,
    "size": "1024x1024"
  }'`,
  edit: `curl ${base.value}/v1/images/edits \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  -H "Idempotency-Key: edit-order-001" \\\n  -F "model=byteplus-nano-banana-pro" \\\n  -F "prompt=Replace the background with a studio backdrop" \\\n  -F "image=@reference.png"`,
  imageAsync: `curl ${base.value}/v1/images/generations \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  -H "Content-Type: application/json" \\\n  -H "Idempotency-Key: image-order-002" \\\n  -H "Prefer: respond-async" \\\n  -d '{"model":"seedream-5.0-pro","prompt":"A cinematic city"}'

curl "${base.value}/v1/images/tasks?request_id=image-order-002" \\\n  -H "Authorization: Bearer sk-your-api-key"`,
  video: `curl ${base.value}/v1/videos \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  -H "Content-Type: application/json" \\\n  -H "Idempotency-Key: video-order-001" \\\n  -d '{
    "model": "kling-3",
    "prompt": "Slow dolly through a neon alley",
    "seconds": 8,
    "size": "1280x720"
  }'

curl ${base.value}/v1/videos/VIDEO_ID \\\n  -H "Authorization: Bearer sk-your-api-key"

curl ${base.value}/v1/videos/VIDEO_ID/content \\\n  -H "Authorization: Bearer sk-your-api-key" \\\n  --output result.mp4`,
}))

async function copy(name) {
  if (await copyText(examples.value[name])) {
    copied.value = name
    setTimeout(() => { if (copied.value === name) copied.value = '' }, 1600)
  }
}
</script>

<template>
  <section class="space-y-6">
    <div>
      <h2 class="text-xl font-semibold text-white/90">2API 接入文档</h2>
      <p class="mt-1 text-xs text-white/40">文本、图片和视频统一使用 OpenAI Bearer 鉴权。多渠道产品使用带渠道前缀的 canonical 模型 ID。</p>
    </div>

    <div class="callout">
      <Icon name="shield" class="w-5 h-5 text-emerald-300 shrink-0" />
      <div><div class="text-sm font-semibold text-white/85">Authorization</div><code class="block mt-1 text-xs text-emerald-300">Authorization: Bearer sk-your-api-key</code><p class="mt-2 text-[11px] text-white/40">不支持 <code>x-api-key</code>、Query、Cookie 或其他自定义鉴权头。</p></div>
    </div>

    <nav class="card p-4 flex flex-wrap gap-2 text-xs">
      <a v-for="link in [['模型','#models'],['文本','#chat'],['生图','#images'],['改图','#edits'],['视频','#videos'],['错误','#errors']]" :key="link[1]" :href="link[1]" class="rounded-lg px-3 py-1.5 bg-white/[0.04] text-white/55 hover:text-white hover:bg-white/[0.08]">{{ link[0] }}</a>
    </nav>

    <article id="models" class="doc-section">
      <div class="doc-title"><span class="get">GET</span><code>/v1/models</code></div>
      <p class="doc-text">只返回下表 {{ TEXT_MODELS.length + IMAGE_MODELS.length + VIDEO_MODELS.length }} 个闭集 ID，<code>owned_by</code> 固定为 <code>2api</code>。默认返回严格 OpenAI 模型对象；需要比例、分辨率等 2API 能力字段时显式使用 <code>?extended=true</code>。同一产品若有多个渠道，公开 ID 带渠道前缀（例如 <code>gpt-image-2</code> 与 <code>byteplus-gpt-image-2</code>）。内部 route 和上游模型 ID 不能作为请求的 <code>model</code> 值。</p>
      <div class="grid lg:grid-cols-3 gap-3">
        <div class="model-group"><h4>文本 · {{ TEXT_MODELS.length }}</h4><code v-for="id in TEXT_MODELS" :key="id">{{ id }}</code></div>
        <div class="model-group"><h4>图片 · {{ IMAGE_MODELS.length }}</h4><code v-for="id in IMAGE_MODELS" :key="id">{{ id }}</code></div>
        <div class="model-group"><h4>视频 · {{ VIDEO_MODELS.length }}</h4><code v-for="id in VIDEO_MODELS" :key="id">{{ id }}</code></div>
      </div>
      <div class="code-block"><button @click="copy('models')">{{ copied === 'models' ? '已复制' : '复制' }}</button><pre>{{ examples.models }}</pre></div>
    </article>

    <article id="chat" class="doc-section">
      <div class="doc-title"><span class="post">POST</span><code>/v1/chat/completions</code></div>
      <p class="doc-text">兼容 OpenAI Chat Completions，支持 <code>stream: true</code> 的 SSE 输出。响应中的 <code>model</code> 始终是 canonical ID。</p>
      <div class="code-block"><button @click="copy('chat')">{{ copied === 'chat' ? '已复制' : '复制' }}</button><pre>{{ examples.chat }}</pre></div>
    </article>

    <article id="images" class="doc-section">
      <div class="doc-title"><span class="post">POST</span><code>/v1/images/generations</code></div>
      <p class="doc-text">JSON 字段：<code>model</code>、<code>prompt</code>、<code>n</code>、<code>size</code>、<code>quality</code>、<code>response_format</code>。路由会按比例、分辨率和完整能力组合筛选。</p>
      <div class="code-block"><button @click="copy('image')">{{ copied === 'image' ? '已复制' : '复制' }}</button><pre>{{ examples.image }}</pre></div>
      <h4 class="mt-5 text-xs font-semibold text-white/70">异步生图</h4><p class="doc-text">添加 <code>Prefer: respond-async</code> 获得 202，再用同一 API Key 查询 <code>GET /v1/images/tasks?request_id=...</code>。</p>
      <div class="code-block"><button @click="copy('imageAsync')">{{ copied === 'imageAsync' ? '已复制' : '复制' }}</button><pre>{{ examples.imageAsync }}</pre></div>
    </article>

    <article id="edits" class="doc-section">
      <div class="doc-title"><span class="post">POST</span><code>/v1/images/edits</code></div>
      <p class="doc-text">必须使用 <code>multipart/form-data</code>，至少上传一个 PNG、JPEG、GIF 或 WebP 格式的 <code>image</code> / <code>image[]</code>。当前不支持 <code>mask</code>、<code>background</code> 与 <code>output_format</code>，传入时会明确返回参数错误。</p>
      <div class="code-block"><button @click="copy('edit')">{{ copied === 'edit' ? '已复制' : '复制' }}</button><pre>{{ examples.edit }}</pre></div>
    </article>

    <article id="videos" class="doc-section">
      <div class="doc-title"><span class="post">POST</span><code>/v1/videos</code></div>
      <p class="doc-text">创建异步视频任务，JSON 支持 <code>model</code>、<code>prompt</code>、<code>seconds</code>、<code>size</code>。带参考素材时使用 multipart：<code>input_reference</code>、<code>reference_videos</code>、<code>reference_audios</code>。</p>
      <div class="endpoint-list"><span class="get">GET</span><code>/v1/videos/:id</code><span>查询状态</span><span class="get">GET</span><code>/v1/videos/:id/content</code><span>下载成品</span></div>
      <div class="code-block"><button @click="copy('video')">{{ copied === 'video' ? '已复制' : '复制' }}</button><pre>{{ examples.video }}</pre></div>
    </article>

    <article id="errors" class="doc-section">
      <h3 class="text-sm font-semibold text-white/85">OpenAI 错误格式</h3>
      <pre class="simple-code">{"error":{"message":"...","type":"invalid_request_error","param":"model","code":"model_not_found"}}</pre>
      <div class="mt-3 grid sm:grid-cols-2 gap-2 text-[11px] text-white/45"><div class="error-item"><strong>401</strong><span><code>invalid_api_key</code> — Key 无效或已吊销</span></div><div class="error-item"><strong>404</strong><span><code>model_not_found</code> — 非 canonical 模型</span></div><div class="error-item"><strong>409</strong><span>同幂等 Key 但请求指纹不同</span></div><div class="error-item"><strong>429</strong><span>API Key 并发超限或无调度容量</span></div></div>
    </article>

    <div class="callout text-[11px] text-white/45">
      <Icon name="info" class="w-4 h-4 shrink-0 text-amber-300" /><p><code>Idempotency-Key</code> 在同一 API Key 下隔离。一旦上游已受理或提交结果未知，2API 不会换号、换 route 或重提。图片请求未提供时会在响应的 <code>Idempotency-Key</code> / <code>Location</code> 头返回本次生成的恢复句柄；跨重试去重仍应由客户端主动复用稳定的 Key。</p>
    </div>
  </section>
</template>

<style scoped>
.callout { display: flex; gap: .8rem; padding: 1rem; border-radius: .85rem; background: rgb(16 185 129 / .08); box-shadow: inset 0 0 0 1px rgb(16 185 129 / .18); }
.doc-section { scroll-margin-top: 1rem; display: flex; flex-direction: column; gap: .85rem; padding: 1.25rem; border-radius: .9rem; background: var(--surface-2); box-shadow: inset 0 0 0 1px var(--hairline); }
.doc-title { display: flex; align-items: center; gap: .6rem; }
.doc-title code { font-size: .82rem; color: var(--fg); }
.get, .post { display: inline-flex; border-radius: .35rem; padding: .18rem .4rem; font: bold .58rem ui-monospace, SFMono-Regular, monospace; }
.get { color: rgb(125 211 252); background: rgb(14 165 233 / .12); }
.post { color: rgb(167 243 208); background: rgb(16 185 129 / .12); }
.doc-text { font-size: .72rem; line-height: 1.75; color: var(--fg-3); overflow-wrap: anywhere; }
.doc-text code, .callout code { padding: .08rem .28rem; border-radius: .25rem; color: var(--fg); background: color-mix(in srgb, var(--fg) 8%, transparent); }
.model-group { display: flex; flex-direction: column; gap: .45rem; padding: .8rem; border-radius: .7rem; background: var(--surface); box-shadow: inset 0 0 0 1px var(--hairline); }
.model-group h4 { font-size: .65rem; color: var(--fg-3); }
.model-group code { font-size: .68rem; color: rgb(196 181 253); }
.code-block { position: relative; border-radius: .7rem; background: #090b11; box-shadow: inset 0 0 0 1px rgb(255 255 255 / .07); overflow: auto; }
.code-block button { position: absolute; right: .6rem; top: .6rem; padding: .25rem .5rem; border-radius: .35rem; font-size: .6rem; color: rgb(255 255 255 / .5); background: rgb(255 255 255 / .07); }
.code-block pre, .simple-code { padding: 1rem; padding-right: 4rem; white-space: pre-wrap; word-break: break-word; font: .68rem/1.65 ui-monospace, SFMono-Regular, Menlo, monospace; color: rgb(167 243 208); }
.endpoint-list { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; gap: .5rem .7rem; font-size: .68rem; color: var(--fg-3); }
.endpoint-list code { color: var(--fg); }
.simple-code { padding: 1rem; border-radius: .65rem; background: #090b11; }
.error-item { display: flex; gap: .6rem; padding: .65rem; border-radius: .55rem; background: var(--surface); box-shadow: inset 0 0 0 1px var(--hairline); }
.error-item strong { color: rgb(253 164 175); }
</style>
