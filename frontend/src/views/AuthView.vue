<script setup>
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import Logo from '../components/Logo.vue'
import { initializeAdmin, login } from '../auth'

const props = defineProps({ mode: { type: String, required: true } })
const router = useRouter()
const initializing = computed(() => props.mode === 'initialize')
const busy = ref(false)
const error = ref('')
const form = reactive({ username: '', email: '', password: '', confirm: '', bootstrapToken: '' })

async function submit() {
  error.value = ''
  if (!form.username.trim() || !form.password) {
    error.value = initializing.value ? '请填写管理员用户名和密码' : '请填写账号和密码'
    return
  }
  if (initializing.value && !form.bootstrapToken.trim()) {
    error.value = '请填写部署环境中的初始化令牌'
    return
  }
  if (initializing.value && form.password !== form.confirm) {
    error.value = '两次输入的密码不一致'
    return
  }
  busy.value = true
  try {
    if (initializing.value) {
      await initializeAdmin({
        username: form.username.trim(),
        email: form.email.trim(),
        password: form.password,
        bootstrapToken: form.bootstrapToken.trim(),
      })
    } else {
      await login(form.username.trim(), form.password)
    }
    form.password = ''
    form.confirm = ''
    form.bootstrapToken = ''
    await router.replace('/admin/overview')
  } catch (e) {
    error.value = e?.message || '操作失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="min-h-screen aurora relative overflow-hidden text-[color:var(--fg)] grid place-items-center px-4 py-10">
    <section class="auth-card">
      <!-- brand panel -->
      <div class="brand-panel">
        <div class="glow glow-a" aria-hidden="true"></div>
        <div class="glow glow-b" aria-hidden="true"></div>
        <div class="brand-inner">
          <div class="flex items-center gap-3">
            <Logo :size="38" />
            <span class="brand-name">Nexa</span>
          </div>
          <h2 class="brand-headline">统一的多模态<br />API 网关控制台</h2>
          <ul class="brand-features">
            <li><Icon name="plug" class="w-4 h-4" /><span>多渠道账号池，智能额度调度</span></li>
            <li><Icon name="shield" class="w-4 h-4" /><span>OpenAI 兼容接口，Bearer 鉴权</span></li>
            <li><Icon name="files" class="w-4 h-4" /><span>文本 · 图片 · 视频全链路日志</span></li>
          </ul>
          <p class="mono brand-foot">TEXT · IMAGE · VIDEO — ONE GATEWAY</p>
        </div>
      </div>

      <!-- form panel -->
      <div class="form-panel">
        <div class="md:hidden flex items-center gap-3 mb-6">
          <Logo :size="34" />
          <span class="text-base font-bold tracking-tight">Nexa</span>
        </div>
        <h1 class="form-title">{{ initializing ? '初始化控制台' : '欢迎回来' }}</h1>
        <p class="form-sub">{{ initializing ? '创建唯一的超级管理员，此操作仅可在首次部署时执行。' : '仅超级管理员可访问控制台。' }}</p>

        <form class="mt-7 space-y-4" @submit.prevent="submit">
          <label v-if="initializing" class="block">
            <span class="form-label">初始化令牌</span>
            <input
              v-model="form.bootstrapToken"
              type="password"
              class="field mt-1.5"
              autocomplete="off"
              placeholder="ADMIN_BOOTSTRAP_TOKEN"
            />
            <span class="block mt-1.5 text-[11px] leading-4 text-[color:var(--fg-faint)]">
              填写部署环境中配置的一次性初始化令牌。
            </span>
          </label>
          <label class="block">
            <span class="form-label">{{ initializing ? '管理员用户名' : '账号' }}</span>
            <input v-model="form.username" class="field mt-1.5" autocomplete="username" :placeholder="initializing ? 'admin' : '用户名或邮箱'" autofocus />
          </label>
          <label v-if="initializing" class="block">
            <span class="form-label">邮箱 <span class="text-[color:var(--fg-faint)]">（可选）</span></span>
            <input v-model="form.email" type="email" class="field mt-1.5" autocomplete="email" placeholder="admin@example.com" />
          </label>
          <div class="grid gap-4" :class="initializing ? 'sm:grid-cols-2' : ''">
            <label class="block">
              <span class="form-label">密码</span>
              <input v-model="form.password" type="password" class="field mt-1.5" :autocomplete="initializing ? 'new-password' : 'current-password'" placeholder="至少 12 位" />
            </label>
            <label v-if="initializing" class="block">
              <span class="form-label">确认密码</span>
              <input v-model="form.confirm" type="password" class="field mt-1.5" autocomplete="new-password" />
            </label>
          </div>

          <p v-if="error" class="alert">{{ error }}</p>
          <button class="btn-primary w-full justify-center mt-1" :disabled="busy">
            <span v-if="busy" class="w-3.5 h-3.5 rounded-full border-2 border-current border-t-transparent animate-spin"></span>
            {{ busy ? '请稍候…' : (initializing ? '创建管理员' : '登 录') }}
          </button>
        </form>

        <p class="mt-6 text-[11px] leading-5 text-[color:var(--fg-faint)] text-center">
          会话保存在 HttpOnly Cookie 中，浏览器脚本无法读取管理员凭据。
        </p>
      </div>
    </section>
  </main>
</template>

<style scoped>
.auth-card {
  display: grid; width: 100%; max-width: 880px;
  grid-template-columns: 1fr;
  background: var(--surface);
  border: 1px solid var(--hairline);
  border-radius: 24px;
  overflow: hidden;
  box-shadow: 0 32px 80px -24px rgb(9 9 12 / .3);
}
@media (min-width: 768px) { .auth-card { grid-template-columns: 1fr 1fr; } }

/* ---- brand panel: constant deep-space gradient ---- */
.brand-panel {
  position: relative; display: none; overflow: hidden;
  background: linear-gradient(160deg, #0b0b12 0%, #0d1020 100%);
  padding: 40px 36px;
}
@media (min-width: 768px) { .brand-panel { display: block; } }
.glow { position: absolute; border-radius: 999px; filter: blur(70px); pointer-events: none; }
.glow-a { width: 300px; height: 300px; top: -80px; right: -60px; background: rgb(34 211 238 / .16); }
.glow-b { width: 280px; height: 280px; bottom: -70px; left: -60px; background: rgb(99 102 241 / .16); }
.brand-inner { position: relative; height: 100%; display: flex; flex-direction: column; }
.brand-name { color: #fafafa; font-size: 18px; font-weight: 700; letter-spacing: -0.01em; }
.brand-headline {
  margin: auto 0 0; color: #fafafa;
  font-size: 26px; font-weight: 700; line-height: 1.35; letter-spacing: -0.02em;
}
.brand-features { list-style: none; margin: 28px 0 0; padding: 0; display: grid; gap: 14px; }
.brand-features li { display: flex; align-items: center; gap: 11px; color: rgb(250 250 250 / .68); font-size: 13px; }
.brand-features svg { color: #22d3ee; flex: none; }
.brand-foot { margin: 32px 0 0; font-size: 9px; letter-spacing: .28em; color: rgb(250 250 250 / .3); }

/* ---- form panel ---- */
.form-panel { padding: 36px 32px; }
@media (min-width: 768px) { .form-panel { padding: 44px 40px; } }
.form-title { margin: 0; font-size: 22px; font-weight: 700; letter-spacing: -0.02em; color: var(--fg); }
.form-sub { margin: 8px 0 0; font-size: 13px; line-height: 1.6; color: var(--fg-3); }
.form-label {
  display: block; font-size: 12px; font-weight: 600;
  letter-spacing: .01em; color: var(--fg-2);
}
.alert {
  border: 1px solid rgb(220 38 38 / .3); background: rgb(220 38 38 / .07);
  color: var(--bad);
  padding: .65rem .85rem; font-size: 12px; border-radius: 10px;
}
</style>
