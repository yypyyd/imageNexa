<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import Icon from './Icon.vue'

const sizes = [20, 50, 100, 200]

const props = defineProps({
  page: { type: Number, required: true },
  pages: { type: Number, required: true },
  total: { type: Number, required: true },
  limit: { type: Number, default: 20 },
})

const emit = defineEmits(['update:page', 'update:limit'])

const items = computed(() => {
  const last = Math.max(1, Number(props.pages) || 1)
  const current = Number(props.page) || 1
  if (last <= 7) return Array.from({ length: last }, (_, index) => index + 1)
  const wanted = new Set([1, last, current - 1, current, current + 1])
  if (current <= 4) [2, 3, 4, 5].forEach((value) => wanted.add(value))
  if (current >= last - 3) [last - 4, last - 3, last - 2, last - 1].forEach((value) => wanted.add(value))
  const ordered = [...wanted].filter((value) => value >= 1 && value <= last).sort((a, b) => a - b)
  return ordered.flatMap((value, index) => (index > 0 && value - ordered[index - 1] > 1 ? [`gap-${value}`, value] : [value]))
})

const jump = ref(props.page)
watch(() => props.page, (value) => { jump.value = value })

const sizeOpen = ref(false)
const sizeRoot = ref(null)

function go(target) {
  const next = Math.max(1, Math.min(Math.max(1, props.pages), Number(target) || 1))
  if (next === props.page) return
  emit('update:page', next)
}

function submitJump() {
  go(jump.value)
}

function setLimit(value) {
  sizeOpen.value = false
  const next = Number(value) || props.limit
  if (next === props.limit) return
  emit('update:limit', next)
}

function onDocClick(event) {
  if (sizeOpen.value && sizeRoot.value && !sizeRoot.value.contains(event.target)) sizeOpen.value = false
}

onMounted(() => document.addEventListener('mousedown', onDocClick))
onUnmounted(() => document.removeEventListener('mousedown', onDocClick))
</script>

<template>
  <div class="pager">
    <span class="total">共 {{ Number(total || 0).toLocaleString('zh-CN') }} 条</span>
    <div ref="sizeRoot" class="size">
      <button type="button" class="size-btn" @click="sizeOpen = !sizeOpen">
        {{ limit }} 条/页
        <Icon name="chevron" class="chev" :class="sizeOpen && 'open'" />
      </button>
      <div v-if="sizeOpen" class="size-menu">
        <button v-for="n in sizes" :key="n" type="button" :class="n === limit && 'on'" @click="setLimit(n)">{{ n }} 条/页</button>
      </div>
    </div>
    <nav class="pages" aria-label="分页">
      <button type="button" class="arrow" :disabled="page <= 1" title="上一页" @click="go(page - 1)"><Icon name="chevron" class="prev" /></button>
      <template v-for="item in items" :key="item">
        <span v-if="typeof item !== 'number'" class="gap">…</span>
        <button v-else type="button" class="num" :class="item === page && 'on'" :aria-current="item === page ? 'page' : undefined" @click="go(item)">{{ item }}</button>
      </template>
      <button type="button" class="arrow" :disabled="page >= pages" title="下一页" @click="go(page + 1)"><Icon name="chevron" class="next" /></button>
    </nav>
    <label class="goto">前往 <input v-model.number="jump" type="number" min="1" :max="pages" @keyup.enter="submitJump" @change="submitJump" /></label>
  </div>
</template>

<style scoped>
.pager { display: flex; align-items: center; justify-content: flex-end; gap: .65rem; flex-wrap: wrap; color: var(--fg-3); font-size: .72rem; }
.total { margin-right: auto; }
.size { position: relative; }
.size-btn { display: inline-flex; align-items: center; gap: .35rem; height: 1.9rem; padding: 0 .6rem; border-radius: .4rem; color: var(--fg-2); background: var(--surface); box-shadow: inset 0 0 0 1px var(--hairline); font-size: .68rem; }
.size-btn:hover { color: var(--fg); }
.chev { width: .85rem; height: .85rem; transition: transform .15s; }
.chev.open { transform: rotate(180deg); }
.size-menu { position: absolute; left: 0; bottom: calc(100% + .35rem); z-index: 20; min-width: 100%; padding: .25rem; border-radius: .5rem; background: var(--menu-bg, var(--surface)); box-shadow: 0 10px 30px rgb(0 0 0 / .18), inset 0 0 0 1px var(--hairline); }
.size-menu button { display: block; width: 100%; padding: .38rem .55rem; border-radius: .35rem; color: var(--fg-2); font-size: .68rem; text-align: left; }
.size-menu button:hover { background: var(--hover); color: var(--fg); }
.size-menu button.on { color: rgb(124 58 237); font-weight: 600; }
.pages { display: flex; align-items: center; gap: .15rem; }
.arrow, .num { display: inline-grid; place-items: center; min-width: 1.7rem; height: 1.7rem; padding: 0 .2rem; color: var(--fg-2); font-size: .72rem; background: transparent; }
.arrow:hover:not(:disabled), .num:hover:not(.on) { color: var(--fg); }
.arrow:disabled { opacity: .3; }
.num.on { color: rgb(124 58 237); font-weight: 700; }
.arrow svg { width: .85rem; height: .85rem; }
.prev { transform: rotate(90deg); }
.next { transform: rotate(-90deg); }
.gap { padding: 0 .15rem; color: var(--fg-faint); }
.goto { display: inline-flex; align-items: center; gap: .4rem; }
.goto input { width: 2.6rem; height: 1.9rem; border: 1px solid var(--hairline); border-radius: .4rem; padding: 0 .35rem; color: var(--fg); background: var(--surface); font-size: .72rem; text-align: center; outline: none; appearance: textfield; }
.goto input::-webkit-outer-spin-button,
.goto input::-webkit-inner-spin-button { appearance: none; }
.goto input:focus { border-color: rgb(139 92 246 / .65); box-shadow: 0 0 0 2px rgb(139 92 246 / .12); }
</style>
