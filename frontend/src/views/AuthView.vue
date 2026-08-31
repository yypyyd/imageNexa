<script setup>
import { computed, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
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
  <main class="min-h-screen relative overflow-hidden bg-[var(--app-bg)] text-[color:var(--fg)] grid place-items-center px-4">
    <div aria-hidden="true" class="absolute inset-0 pointer-events-none">
      <div class="absolute -top-36 left-1/4 w-[34rem] h-[34rem] rounded-full bg-violet-500/15 blur-[120px]"></div>
      <div class="absolute -bottom-36 right-1/4 w-[32rem] h-[32rem] rounded-full bg-cyan-500/10 blur-[120px]"></div>
    </div>

    <section class="relative w-full max-w-md rounded-3xl border border-[color:var(--hairline)] bg-[var(--surface)] backdrop-blur-xl shadow-2xl shadow-black/10 p-8">
      <div class="flex items-center gap-3 mb-7">
        <div class="w-11 h-11 rounded-2xl bg-gradient-to-br from-violet-500 to-fuchsia-500 grid place-items-center text-white shadow-lg shadow-violet-500/20">
          <Icon name="shield" class="w-5 h-5" />
        </div>
        <div>
          <h1 class="font-semibold tracking-tight">{{ initializing ? '初始化 2API' : '登录 2API' }}</h1>
          <p class="text-xs text-[color:var(--fg-3)] mt-0.5">{{ initializing ? '创建唯一的超级管理员' : '仅超级管理员可访问' }}</p>
        </div>
      </div>

      <form class="space-y-4" @submit.prevent="submit">
        <label v-if="initializing" class="block">
          <span class="auth-label">初始化令牌</span>
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
          <span class="auth-label">{{ initializing ? '管理员用户名' : '账号' }}</span>
          <input v-model="form.username" class="field mt-1.5" autocomplete="username" :placeholder="initializing ? 'admin' : '用户名或邮箱'" autofocus />
        </label>
        <label v-if="initializing" class="block">
          <span class="auth-label">邮箱 <span class="text-[color:var(--fg-faint)]">（可选）</span></span>
          <input v-model="form.email" type="email" class="field mt-1.5" autocomplete="email" placeholder="admin@example.com" />
        </label>
        <label class="block">
          <span class="auth-label">密码</span>
          <input v-model="form.password" type="password" class="field mt-1.5" :autocomplete="initializing ? 'new-password' : 'current-password'" placeholder="至少 12 位" />
        </label>
        <label v-if="initializing" class="block">
          <span class="auth-label">确认密码</span>
          <input v-model="form.confirm" type="password" class="field mt-1.5" autocomplete="new-password" />
        </label>

        <p v-if="error" class="rounded-xl bg-rose-500/10 ring-1 ring-rose-400/25 px-3 py-2.5 text-xs text-rose-500">{{ error }}</p>
        <button class="btn-primary w-full justify-center !py-2.5" :disabled="busy">
          <span v-if="busy" class="w-3.5 h-3.5 rounded-full border-2 border-current border-t-transparent animate-spin"></span>
          {{ busy ? '请稍候…' : (initializing ? '创建管理员' : '登录') }}
        </button>
      </form>

      <p class="mt-6 text-[11px] leading-5 text-[color:var(--fg-faint)] text-center">
        会话保存在 HttpOnly Cookie 中，浏览器脚本无法读取管理员凭据。
      </p>
    </section>
  </main>
</template>

<style scoped>
.auth-label { display: block; font-size: 0.72rem; font-weight: 600; color: var(--fg-2); }
</style>
