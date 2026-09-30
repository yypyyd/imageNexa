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
  <div class="shell aurora">
    <div v-if="mobileOpen" class="scrim md:hidden" @click="mobileOpen = false"></div>
    <aside class="sider" :class="mobileOpen && 'open'">
      <router-link to="/admin/overview" class="brand" @click="mobileOpen = false">
        <Logo :size="32" />
        <span class="brand-text">
          <b>Nexa</b>
          <i>Gateway Console</i>
        </span>
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
      <div class="sider-foot">
        <span class="beat" aria-hidden="true"></span>
        <span class="foot-text">所有系统运行正常</span>
      </div>
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
.scrim { position: fixed; inset: 0; z-index: 30; background: rgb(9 9 12 / .5); backdrop-filter: blur(2px); }
.sider {
  position: fixed; inset: 10px auto 10px 10px; z-index: 40; width: 232px;
  display: flex; flex-direction: column;
  background: var(--sider-bg);
  backdrop-filter: blur(20px);
  border: 1px solid var(--hairline);
  border-radius: 18px;
  box-shadow: var(--shadow-card);
  transform: translateX(calc(-100% - 20px));
  transition: transform .25s cubic-bezier(.4, 0, .2, 1);
}
.sider.open { transform: translateX(0); }
@media (min-width: 768px) {
  .shell { padding: 12px; gap: 12px; }
  .sider { position: sticky; inset: auto; flex: none; height: calc(100vh - 24px); transform: none; }
}
.brand {
  height: 68px; flex: none;
  display: flex; align-items: center; gap: 12px;
  padding: 0 18px;
}
.brand-text { display: flex; flex-direction: column; line-height: 1.1; }
.brand-text b { color: var(--fg); font-size: 17px; font-weight: 700; letter-spacing: -0.01em; }
.brand-text i { font-style: normal; font-family: var(--font-mono); font-size: 9px; letter-spacing: .18em; color: var(--fg-faint); margin-top: 3px; }
.menu { flex: 1; overflow: auto; padding: 4px 10px; }
.item {
  display: flex; align-items: center; gap: 11px;
  height: 40px; padding: 0 12px; margin-bottom: 2px;
  border-radius: 10px;
  color: var(--sider-fg); font-size: 13.5px; font-weight: 500;
  transition: background-color .15s ease, color .15s ease;
}
.item:hover { color: var(--fg); background: var(--sider-hover); }
.item.active { color: var(--fg); background: var(--accent-soft); }
.item-icon { width: 17px; height: 17px; flex: none; opacity: .75; }
.item.active .item-icon { opacity: 1; color: var(--accent); }
.sider-foot {
  flex: none;
  display: flex; align-items: center; gap: 9px;
  margin: 10px; padding: 10px 12px;
  border-radius: 12px;
  background: var(--surface-2);
  border: 1px solid var(--hairline-soft);
}
.beat {
  width: 8px; height: 8px; border-radius: 999px; flex: none;
  background: var(--ok);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--ok) 18%, transparent);
  animation: beat 2.4s ease-in-out infinite;
}
@keyframes beat { 0%, 100% { opacity: 1; } 50% { opacity: .45; } }
.foot-text { font-size: 11.5px; color: var(--fg-3); }
.main {
  flex: 1; min-width: 0; display: flex; flex-direction: column;
  border-radius: 18px;
  border: 1px solid var(--hairline);
  background: var(--app-bg);
  overflow: hidden;
}
@media (max-width: 767px) { .main { border-radius: 0; border: none; } }
.header {
  height: 60px; flex: none;
  display: flex; align-items: center; gap: 12px;
  padding: 0 20px;
  background: var(--header-bg);
  backdrop-filter: blur(14px);
  border-bottom: 1px solid var(--hairline-soft);
}
.header h1 {
  margin: 0; flex: 1; min-width: 0;
  font-size: 15px; font-weight: 650; letter-spacing: -0.01em;
  color: var(--fg);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.header-right { display: flex; align-items: center; gap: 6px; }
.ghost {
  width: 36px; height: 36px; border-radius: 10px;
  display: grid; place-items: center; color: var(--fg-2);
  transition: background-color .15s ease, color .15s ease;
}
.ghost:hover { background: var(--hover); color: var(--fg); }
.ghost.menu-btn { display: none; }
@media (max-width: 767px) { .ghost.menu-btn { display: grid; } }
.user { position: relative; }
.user-btn {
  display: flex; align-items: center; gap: 9px;
  height: 36px; padding: 0 10px 0 5px; border-radius: 10px;
  color: var(--fg);
  transition: background-color .15s ease;
}
.user-btn:hover { background: var(--hover); }
.avatar {
  width: 27px; height: 27px; border-radius: 999px;
  display: grid; place-items: center;
  font-size: 11.5px; font-weight: 650; color: #fff;
  background: linear-gradient(135deg, #06b6d4, #6366f1);
}
.user-name { max-width: 8rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 500; }
.chevron { width: 14px; height: 14px; color: var(--fg-3); transition: transform .15s; }
.chevron.up { transform: rotate(180deg); }
.dropdown {
  position: absolute; right: 0; top: calc(100% + 8px); z-index: 20;
  min-width: 152px; padding: 6px;
  background: var(--menu-bg); color: var(--fg);
  border: 1px solid var(--hairline);
  border-radius: 14px;
  box-shadow: 0 16px 40px -12px rgb(9 9 12 / .25);
}
.dropdown button {
  width: 100%; display: flex; align-items: center; gap: 8px;
  height: 36px; padding: 0 10px; border-radius: 9px;
  font-size: 13.5px; color: var(--fg-2);
}
.dropdown button:hover { background: var(--hover); color: var(--fg); }
.content { flex: 1; overflow: auto; }
.content-inner { max-width: 1600px; margin: 0 auto; padding: 20px; }
@media (min-width: 768px) { .content-inner { padding: 26px 30px 52px; } }
</style>
