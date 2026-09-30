<script setup>
// Custom dropdown that replaces the native <select> so the OPEN list is themed
// too (native option popups can't be styled). Trigger mirrors the `.field` look;
// the panel is a floating dark menu with hover + selected states. Keyboard:
// Enter/Space/↑/↓ open, ↑/↓ move, Enter select, Esc close.
import { ref, computed, nextTick, onMounted, onUnmounted } from 'vue'
import Icon from './Icon.vue'

const props = defineProps({
  modelValue: { type: [String, Number], default: '' },
  // [{ value, label }]
  options: { type: Array, default: () => [] },
  placeholder: { type: String, default: '请选择' },
  mono: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
})
const emit = defineEmits(['update:modelValue'])

const open = ref(false)
const root = ref(null)
const active = ref(-1)   // keyboard-highlighted index

const selected = computed(() => props.options.find((o) => o.value === props.modelValue) || null)
const label = computed(() => (selected.value ? selected.value.label : props.placeholder))

function toggle() {
  if (props.disabled) return
  open.value ? close() : openMenu()
}
function openMenu() {
  open.value = true
  active.value = Math.max(0, props.options.findIndex((o) => o.value === props.modelValue))
  nextTick(scrollActiveIntoView)
}
function close() {
  open.value = false
  active.value = -1
}
function pick(opt) {
  emit('update:modelValue', opt.value)
  close()
}

function onKeydown(e) {
  if (!open.value) {
    if (['Enter', ' ', 'ArrowDown', 'ArrowUp'].includes(e.key)) { e.preventDefault(); openMenu() }
    return
  }
  if (e.key === 'Escape') { e.preventDefault(); close() }
  else if (e.key === 'ArrowDown') { e.preventDefault(); active.value = Math.min(props.options.length - 1, active.value + 1); scrollActiveIntoView() }
  else if (e.key === 'ArrowUp') { e.preventDefault(); active.value = Math.max(0, active.value - 1); scrollActiveIntoView() }
  else if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (props.options[active.value]) pick(props.options[active.value]) }
}

const panel = ref(null)
function scrollActiveIntoView() {
  nextTick(() => {
    const el = panel.value?.querySelector(`[data-idx="${active.value}"]`)
    el?.scrollIntoView({ block: 'nearest' })
  })
}

function onDocClick(e) {
  if (open.value && root.value && !root.value.contains(e.target)) close()
}
onMounted(() => document.addEventListener('mousedown', onDocClick))
onUnmounted(() => document.removeEventListener('mousedown', onDocClick))
</script>

<template>
  <div ref="root" class="relative">
    <!-- trigger -->
    <button type="button" @click="toggle" @keydown="onKeydown"
            :aria-expanded="open"
            :disabled="disabled"
            class="field flex items-center justify-between gap-2 text-left h-10 !py-0 disabled:opacity-50 disabled:cursor-not-allowed"
            :class="[mono ? 'font-mono' : '', selected ? '' : 'text-[color:var(--fg-faint)]']">
      <span class="truncate">{{ label }}</span>
      <Icon name="chevron"
            class="w-4 h-4 shrink-0 text-[color:var(--fg-3)] transition-transform duration-200"
            :class="open ? 'rotate-180' : ''" />
    </button>

    <!-- panel -->
    <transition
      enter-active-class="transition duration-150 ease-out"
      enter-from-class="opacity-0 -translate-y-1"
      enter-to-class="opacity-100 translate-y-0"
      leave-active-class="transition duration-100 ease-in"
      leave-from-class="opacity-100 translate-y-0"
      leave-to-class="opacity-0 -translate-y-1">
      <div v-if="open" ref="panel"
           class="absolute z-30 mt-2 w-full max-h-64 overflow-auto rounded-xl border border-[color:var(--hairline)]
                  bg-[var(--menu-bg)] backdrop-blur-xl p-1.5 shadow-2xl shadow-black/20 ring-1 ring-[color:var(--hairline)]">
        <button v-for="(o, i) in options" :key="o.value" type="button"
                :data-idx="i" @click="pick(o)" @mouseenter="active = i"
                class="w-full flex items-center justify-between gap-2 rounded-lg px-3 py-2 text-left text-sm transition-colors"
                :class="[
                  mono ? 'font-mono' : '',
                  i === active ? 'bg-[var(--hover)] text-[color:var(--fg)]' : 'text-[color:var(--fg-2)]',
                ]">
          <span class="truncate">{{ o.label }}</span>
          <Icon v-if="o.value === modelValue" name="check" class="w-4 h-4 shrink-0" style="color: var(--accent)" />
        </button>
        <div v-if="!options.length" class="px-3 py-2 text-xs text-[color:var(--fg-3)]">无选项</div>
      </div>
    </transition>
  </div>
</template>
