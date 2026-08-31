<script setup>
import { ref } from 'vue'

const stalled = ref(false)
setTimeout(() => { stalled.value = true }, 3000)
function retry() { window.location.reload() }
</script>

<template>
  <main class="min-h-screen grid place-items-center bg-[var(--app-bg)] text-[color:var(--fg)]">
    <div class="text-center space-y-4">
      <div class="mx-auto w-11 h-11 rounded-2xl bg-violet-500/15 ring-1 ring-violet-400/25 grid place-items-center">
        <span class="w-4 h-4 rounded-full border-2 border-violet-400 border-t-transparent animate-spin"></span>
      </div>
      <div>
        <p class="font-semibold">2API 管理控制台</p>
        <p class="mt-1 text-xs text-[color:var(--fg-3)]">{{ stalled ? '正在等待后端响应…' : '正在检查系统状态…' }}</p>
      </div>
      <button v-if="stalled" class="btn-soft" @click="retry">重试</button>
    </div>
  </main>
</template>
