<script setup>
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import Logo from '../components/Logo.vue'
import { auth, logout } from '../auth'
import { isDark, toggleTheme } from '../theme'

const route = useRoute()
const router = useRouter()
const mobileOpen = ref(false)
const currentLabel = computed(() => route.meta?.label || '')
const adminName = computed(() => auth.admin?.username || auth.admin?.name || auth.admin?.email || '超级管理员')

const tabs = [
  { label: '概览', to: '/admin/overview', icon: 'overview' },
  { label: '模型', to: '/admin/models', icon: 'models' },
  { label: '账号', to: '/admin/accounts', icon: 'plug' },
  { label: 'API Key', to: '/admin/api-keys', icon: 'shield' },
  { label: '日志 / 成品', to: '/admin/logs', icon: 'files' },
  { label: '违禁词', to: '/admin/banned-words', icon: 'ban' },
  { label: '系统设置', to: '/admin/settings', icon: 'config' },
  { label: 'API 文档', to: '/admin/docs', icon: 'book' },
]

async function signOut() {
  await logout()
  await router.replace('/login')
}
</script>

<template>
  <div class="theme-x h-screen flex bg-[var(--app-bg)] text-[color:var(--fg-2)] overflow-hidden">
    <div v-if="mobileOpen" class="fixed inset-0 z-30 bg-black/50 md:hidden" @click="mobileOpen = false"></div>
    <aside class="fixed md:static inset-y-0 left-0 z-40 w-64 shrink-0 border-r border-[color:var(--hairline)] bg-[var(--surface)] backdrop-blur-xl flex flex-col transition-transform md:translate-x-0"
           :class="mobileOpen ? 'translate-x-0' : '-translate-x-full'">
      <router-link to="/admin/overview" class="h-16 flex items-center gap-3 px-5 border-b border-[color:var(--hairline)]" @click="mobileOpen = false">
        <Logo :size="34" class="rounded-xl shadow-lg shadow-violet-500/20 ring-1 ring-white/10" />
        <div class="leading-tight">
          <div class="text-sm font-semibold tracking-tight text-[color:var(--fg)]">2API</div>
          <div class="text-[10px] uppercase tracking-[0.18em] text-[color:var(--fg-3)]">Control plane</div>
        </div>
      </router-link>

      <nav class="flex-1 px-3 py-4 space-y-1 overflow-y-auto">
        <router-link v-for="tab in tabs" :key="tab.to" :to="tab.to" class="admin-link group" active-class="active" @click="mobileOpen = false">
          <span class="active-bar"></span>
          <Icon :name="tab.icon" class="w-4 h-4 shrink-0 opacity-70 group-hover:opacity-100" />
          <span class="text-sm">{{ tab.label }}</span>
        </router-link>
      </nav>

      <div class="p-3 border-t border-[color:var(--hairline)] space-y-1">
        <div class="px-3 py-2 mb-1 min-w-0">
          <div class="text-[10px] uppercase tracking-wider text-[color:var(--fg-faint)]">当前管理员</div>
          <div class="mt-1 text-xs font-medium text-[color:var(--fg)] truncate" :title="adminName">{{ adminName }}</div>
        </div>
        <button type="button" class="bottom-action" @click="toggleTheme">
          <Icon :name="isDark ? 'spark' : 'overview'" class="w-3.5 h-3.5" />
          {{ isDark ? '亮色模式' : '暗色模式' }}
        </button>
        <button type="button" class="bottom-action text-rose-400" @click="signOut">
          <Icon name="open" class="w-3.5 h-3.5 rotate-180" />
          退出登录
        </button>
      </div>
    </aside>

    <div class="flex-1 min-w-0 flex flex-col relative">
      <div aria-hidden="true" class="pointer-events-none absolute inset-0 overflow-hidden">
        <div class="absolute -top-40 left-1/3 w-[40rem] h-[40rem] rounded-full bg-violet-500/[0.09] blur-[110px]"></div>
        <div class="absolute top-1/2 -right-40 w-[36rem] h-[36rem] rounded-full bg-cyan-500/[0.07] blur-[110px]"></div>
      </div>
      <header class="relative z-10 h-14 shrink-0 border-b border-[color:var(--hairline)] bg-[var(--app-bg)]/70 backdrop-blur-md flex items-center px-4 md:px-8">
        <button class="mr-3 md:hidden" @click="mobileOpen = true"><Icon name="models" class="w-5 h-5" /></button>
        <div class="text-[10px] uppercase tracking-[0.25em] text-[color:var(--fg-3)] font-medium mr-3">2API</div>
        <div class="text-[color:var(--fg-faint)] mr-3">/</div>
        <h1 class="text-sm font-semibold tracking-tight text-[color:var(--fg)]">{{ currentLabel }}</h1>
      </header>
      <main class="theme-text flex-1 overflow-y-auto relative z-10">
        <div class="px-4 py-5 md:px-8 md:py-7 max-w-[1600px] mx-auto">
          <router-view />
        </div>
      </main>
    </div>
  </div>
</template>

<style scoped>
.admin-link { position: relative; display: flex; align-items: center; gap: .75rem; padding: .58rem .875rem; border-radius: .625rem; color: var(--fg-2); font-weight: 500; transition: background .15s, color .15s; }
.admin-link:hover, .admin-link.active { background: var(--hover); color: var(--fg); }
.active-bar { position: absolute; left: 0; top: 50%; width: 3px; height: 1.25rem; border-radius: 0 999px 999px 0; transform: translateY(-50%); opacity: 0; background: linear-gradient(180deg,#f0abfc,#8b5cf6); }
.admin-link.active .active-bar { opacity: 1; }
.bottom-action { width: 100%; display: flex; align-items: center; gap: .625rem; border-radius: .5rem; padding: .5rem .75rem; font-size: .75rem; transition: background .15s; }
.bottom-action:hover { background: var(--hover); color: var(--fg); }
</style>
