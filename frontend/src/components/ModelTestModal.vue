<script setup>
import { computed, onMounted, ref } from 'vue'
import { api, listOf } from '../api'
import AccountTestModal from './AccountTestModal.vue'
import Icon from './Icon.vue'
import SelectMenu from './SelectMenu.vue'

const props = defineProps({
  model: { type: Object, required: true },
  models: { type: Array, default: () => [] },
})
const emit = defineEmits(['close'])

const accounts = ref([])
const selectedID = ref('')
const selectedAccount = ref(null)
const loading = ref(true)
const error = ref('')

const accountOptions = computed(() => accounts.value.map((account) => ({
  value: account.id,
  label: `${account.label || account.email || account.id} · ${account.provider}`,
})))

async function loadAccounts() {
  loading.value = true
  error.value = ''
  const found = []
  let page = 1
  let total = 0
  do {
    const params = new URLSearchParams({ model_id: props.model.id, status: 'active', limit: '200', page: String(page) })
    const response = await api(`/accounts?${params}`)
    if (!response.ok) {
      error.value = response.error
      break
    }
    const items = listOf(response.data)
    found.push(...items)
    total = Number(response.data?.total ?? found.length)
    if (!items.length) break
    page++
  } while (found.length < total && page <= 50)
  accounts.value = found
  if (!error.value && found.length < total) error.value = `可用账号过多，仅加载前 ${found.length} 个`
  loading.value = false
}

function continueTest() {
  selectedAccount.value = accounts.value.find((account) => account.id === selectedID.value) || null
}

onMounted(loadAccounts)
</script>

<template>
  <AccountTestModal v-if="selectedAccount" :account="selectedAccount" :models="models" :initial-model="model.id" @close="emit('close')" />
  <div v-else class="model-test-bg" @click.self="emit('close')">
    <div class="model-test-card">
      <div class="flex items-start justify-between gap-3">
        <div class="min-w-0">
          <h3 class="font-semibold text-white/90">测试模型</h3>
          <p class="mt-1 truncate font-mono text-[11px] text-white/45">{{ model.id }}</p>
        </div>
        <button type="button" class="text-white/45 hover:text-white" @click="emit('close')"><Icon name="close" class="w-4 h-4" /></button>
      </div>
      <p class="text-[11px] leading-5 text-white/45">先明确选择该模型已授权的可用账号。测试固定使用所选账号，不会自动回退到同 Provider 的其他凭据。</p>
      <p v-if="loading" class="text-xs text-white/45">正在加载可用账号…</p>
      <template v-else>
        <SelectMenu v-if="accounts.length" v-model="selectedID" :options="accountOptions" placeholder="选择具体账号" />
        <p v-else class="text-xs text-amber-300">该模型当前没有已启用且授权的可用账号。</p>
        <p v-if="error" class="text-xs text-rose-300">{{ error }}</p>
        <button type="button" class="btn-primary w-full justify-center" :disabled="!selectedID" @click="continueTest"><Icon name="test" class="w-4 h-4" />进入测试</button>
      </template>
    </div>
  </div>
</template>

<style scoped>
.model-test-bg{position:fixed;inset:0;z-index:60;display:grid;place-items:center;overflow-y:auto;padding:1rem;background:rgb(0 0 0 / .7);backdrop-filter:blur(6px)}
.model-test-card{width:100%;max-width:32rem;display:flex;flex-direction:column;gap:1rem;border-radius:1rem;padding:1.25rem;color:rgb(255 255 255 / .75);background:#11131a;box-shadow:0 24px 80px rgb(0 0 0 / .5),inset 0 0 0 1px rgb(255 255 255 / .09)}
</style>
