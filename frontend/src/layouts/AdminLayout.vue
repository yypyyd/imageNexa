<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import Logo from '../components/Logo.vue'
import { auth, logout } from '../auth'
import { isDark, toggleTheme } from '../theme'

const route = useRoute()
const router = useRouter()
const mobileOpen = ref(false)
const userOpen = ref(false)
const userWrap = ref(null)
const currentLabel = computed(() => route.meta?.label || '')
const adminName = computed(() => auth.admin?.username || auth.admin?.name || auth.admin?.email || '超级管理员')
const adminInitial = computed(() => String(adminName.value || '管').trim().slice(0, 1).toUpperCase())

const tabs = [
  { label: '概览', to: '/admin/overview', icon: 'overview' },
  { label: '模型', to: '/admin/models', icon: 'models' },
  { label: '账号', to: '/admin/accounts', icon: 'plug' },
  { label: 'API 密钥', to: '/admin/api-keys', icon: 'shield' },
  { label: '日志 / 成品', to: '/admin/logs', icon: 'files' },
  { label: '违禁词', to: '/admin/banned-words', icon: 'ban' },
  { label: '系统设置', to: '/admin/settings', icon: 'config' },
  { label: 'API 文档', to: '/admin/docs', icon: 'book' },
]

function onDocClick(event) {
  if (!userWrap.value?.contains(event.target)) userOpen.value = false
}

onMounted(() => document.addEventListener('click', onDocClick))
onBeforeUnmount(() => document.removeEventListener('click', onDocClick))

async function signOut() {
  userOpen.value = false
  await logout()
  await router.replace('/login')
}
</script>

<template>
  <div class="shell">
    <div v-if="mobileOpen" class="scrim md:hidden" @click="mobileOpen = false"></div>
    <aside class="sider" :class="mobileOpen && 'open'">
      <router-link to="/admin/overview" class="brand" @click="mobileOpen = false">
        <Logo :size="28" />
        <span>2API</span>
      </router-link>
      <nav class="menu">
        <router-link
          v-for="tab in tabs"
          :key="tab.to"
          :to="tab.to"
          class="item"
          active-class="active"
          @click="mobileOpen = false"
        >
          <Icon :name="tab.icon" class="item-icon" />
          <span>{{ tab.label }}</span>
        </router-link>
      </nav>
    </aside>

    <div class="main">
      <header class="header">
        <button type="button" class="ghost menu-btn" aria-label="打开菜单" @click="mobileOpen = true">
          <Icon name="menu" class="w-5 h-5" />
        </button>
        <h1>{{ currentLabel }}</h1>
        <div class="header-right">
          <button type="button" class="ghost" :title="isDark ? '切换到亮色' : '切换到暗色'" @click="toggleTheme">
            <Icon :name="isDark ? 'sun' : 'moon'" class="w-4 h-4" />
          </button>
          <div ref="userWrap" class="user">
            <button type="button" class="user-btn" @click="userOpen = !userOpen">
              <span class="avatar">{{ adminInitial }}</span>
              <span class="user-name">{{ adminName }}</span>
              <Icon name="chevron" class="chevron" :class="userOpen && 'up'" />
            </button>
            <div v-if="userOpen" class="dropdown">
              <button type="button" @click="signOut">
                <Icon name="logout" class="w-3.5 h-3.5" />
                退出登录
              </button>
            </div>
          </div>
        </div>
      </header>
      <main class="theme-text content">
        <div class="content-inner">
          <router-view />
        </div>
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell { display: flex; height: 100vh; overflow: hidden; background: var(--app-bg); color: var(--fg-2); }
.scrim { position: fixed; inset: 0; z-index: 30; background: rgb(0 0 0 / .45); }
.sider {
  position: fixed; inset: 0 auto 0 0; z-index: 40; width: 208px;
  display: flex; flex-direction: column;
  background: var(--sider-bg);
  transform: translateX(-100%);
  transition: transform .2s ease;
}
.sider.open { transform: translateX(0); }
@media (min-width: 768px) {
  .sider { position: static; transform: none; }
}
.brand {
  height: 56px; flex: none;
  display: flex; align-items: center; gap: 10px;
  padding: 0 20px;
  color: #fff; font-size: 16px; font-weight: 600;
}
.brand :deep(svg) { border-radius: 6px; }
.menu { flex: 1; overflow: auto; padding: 8px; }
.item {
  display: flex; align-items: center; gap: 10px;
  height: 40px; padding: 0 14px; margin-bottom: 4px;
  border-radius: 6px;
  color: var(--sider-fg); font-size: 14px;
}
.item:hover { color: #fff; background: var(--sider-hover); }
.item.active { color: #fff; background: var(--sider-active); }
.item-icon { width: 16px; height: 16px; flex: none; }
.main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
.header {
  height: 56px; flex: none;
  display: flex; align-items: center; gap: 12px;
  padding: 0 20px;
  background: var(--header-bg);
  box-shadow: 0 1px 4px rgb(0 21 41 / .08);
}
.header h1 { margin: 0; flex: 1; font-size: 16px; font-weight: 600; color: var(--fg); }
.header-right { display: flex; align-items: center; gap: 4px; }
.ghost {
  width: 36px; height: 36px; border-radius: 6px;
  display: grid; place-items: center; color: var(--fg-2);
}
.ghost:hover { background: var(--hover); color: var(--fg); }
.ghost.menu-btn { display: none; }
@media (max-width: 767px) { .ghost.menu-btn { display: grid; } }
.user { position: relative; }
.user-btn {
  display: flex; align-items: center; gap: 8px;
  height: 36px; padding: 0 8px 0 4px; border-radius: 6px;
  color: var(--fg);
}
.user-btn:hover { background: var(--hover); }
.avatar {
  width: 28px; height: 28px; border-radius: 50%;
  display: grid; place-items: center;
  font-size: 12px; font-weight: 600; color: #fff;
  background: var(--accent);
}
.user-name { max-width: 8rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 14px; }
.chevron { width: 14px; height: 14px; color: var(--fg-3); transition: transform .15s; }
.chevron.up { transform: rotate(180deg); }
.dropdown {
  position: absolute; right: 0; top: calc(100% + 6px); z-index: 20;
  min-width: 148px; padding: 6px;
  background: var(--menu-bg); color: var(--fg);
  border: 1px solid var(--hairline);
  border-radius: 8px;
  box-shadow: 0 8px 24px rgb(0 0 0 / .12);
}
.dropdown button {
  width: 100%; display: flex; align-items: center; gap: 8px;
  height: 36px; padding: 0 10px; border-radius: 6px;
  font-size: 14px; color: var(--fg-2);
}
.dropdown button:hover { background: var(--hover); color: var(--fg); }
.content { flex: 1; overflow: auto; }
.content-inner { max-width: 1600px; margin: 0 auto; padding: 20px; }
@media (min-width: 768px) { .content-inner { padding: 24px; } }
</style>
