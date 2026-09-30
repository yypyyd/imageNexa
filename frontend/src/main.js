import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import './style.css'
import { auth, bootstrapAuth } from './auth'

import AdminLayout from './layouts/AdminLayout.vue'
import AuthView from './views/AuthView.vue'
import BootstrapView from './views/BootstrapView.vue'

const routes = [
  { path: '/', component: BootstrapView, meta: { label: '启动中' } },
  { path: '/initialize', component: AuthView, props: { mode: 'initialize' }, meta: { label: '初始化' } },
  { path: '/login', component: AuthView, props: { mode: 'login' }, meta: { label: '登录' } },
  {
    path: '/admin',
    component: AdminLayout,
    meta: { requiresAdmin: true },
    children: [
      { path: '', redirect: '/admin/overview' },
      { path: 'overview', component: () => import('./views/OverviewView.vue'), meta: { label: '概览' } },
      { path: 'models', component: () => import('./views/ModelsView.vue'), meta: { label: '模型' } },
      { path: 'accounts', component: () => import('./views/AccountsView.vue'), meta: { label: '账号' } },
      { path: 'api-keys', component: () => import('./views/ApiKeysView.vue'), meta: { label: 'API 密钥' } },
      { path: 'logs', component: () => import('./views/LogsView.vue'), meta: { label: '日志 / 成品' } },
      { path: 'banned-words', component: () => import('./views/BannedWordsView.vue'), meta: { label: '违禁词' } },
      { path: 'settings', component: () => import('./views/ConfigView.vue'), meta: { label: '系统设置' } },
      { path: 'docs', component: () => import('./views/DocsView.vue'), meta: { label: 'API 文档' } },
    ],
  },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

const router = createRouter({ history: createWebHistory(), routes })

router.beforeEach(async (to) => {
  await bootstrapAuth()
  if (to.path === '/') {
    if (auth.initialized === null) return true
    if (auth.initialized === false) return '/initialize'
    return auth.admin ? '/admin/overview' : '/login'
  }
  if (to.path === '/initialize') {
    if (auth.initialized === null) return '/'
    if (auth.initialized === true) return auth.admin ? '/admin/overview' : '/login'
    return true
  }
  if (to.path === '/login') {
    if (auth.initialized === null) return '/'
    if (auth.initialized === false) return '/initialize'
    if (auth.admin) return '/admin/overview'
    return true
  }
  if (to.meta.requiresAdmin && !auth.admin) {
    if (auth.initialized === null) return '/'
    return auth.initialized === false ? '/initialize' : '/login'
  }
  return true
})

router.afterEach((route) => {
  document.title = route.meta?.label ? `Nexa · ${route.meta.label}` : 'Nexa'
})

createApp(App).use(router).mount('#app')
