<script setup>
import { ref } from 'vue'
import Logo from '../components/Logo.vue'

const stalled = ref(false)
setTimeout(() => { stalled.value = true }, 3000)
function retry() { window.location.reload() }
</script>

<template>
  <main class="min-h-screen aurora grid place-items-center text-[color:var(--fg)]">
    <div class="text-center space-y-5">
      <div class="mx-auto w-fit relative">
        <Logo :size="46" />
        <span class="boot-spinner" aria-hidden="true"></span>
      </div>
      <div>
        <p class="text-sm font-semibold">Nexa 管理控制台</p>
        <p class="mt-1.5 text-xs text-[color:var(--fg-3)]">{{ stalled ? '正在等待后端响应…' : '正在检查系统状态…' }}</p>
      </div>
      <button v-if="stalled" class="btn-soft mx-auto" @click="retry">重试</button>
    </div>
  </main>
</template>

<style scoped>
.boot-spinner {
  position: absolute; right: -9px; bottom: -3px;
  width: 15px; height: 15px; border-radius: 999px;
  border: 2px solid var(--accent); border-top-color: transparent;
  animation: spin .8s linear infinite;
  background: var(--app-bg);
}
@keyframes spin { to { transform: rotate(360deg); } }
</style>
