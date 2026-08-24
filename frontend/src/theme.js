// Light/dark theme state. Default is LIGHT. The choice persists in localStorage
// and is reflected as a `dark` class on <html>, which drives the CSS-variable
// palette in style.css (and the conditional `.public-dark` override layer).
import { ref } from 'vue'

const KEY = 'gw_theme'
const mql = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null

function systemTheme() {
  return mql && mql.matches ? 'dark' : 'light'
}

// Precedence: an explicit user choice (localStorage) wins; otherwise follow the
// OS's light/dark setting.
const saved = localStorage.getItem(KEY)
const initial = saved === 'dark' || saved === 'light' ? saved : systemTheme()

const theme = ref(initial)
export const isDark = ref(initial === 'dark')

function apply(t) {
  isDark.value = t === 'dark'
  const el = document.documentElement
  el.classList.toggle('dark', t === 'dark')
}

// Apply at module load so the first paint already matches the resolved choice.
apply(theme.value)

// While the user hasn't made an explicit choice, keep tracking the OS setting
// live (e.g. they flip macOS/Windows to dark mode with the tab open).
if (mql) {
  mql.addEventListener('change', () => {
    if (!localStorage.getItem(KEY)) {
      theme.value = systemTheme()
      apply(theme.value)
    }
  })
}

/** Flip light ⇄ dark and persist as the user's explicit choice. */
export function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark'
  localStorage.setItem(KEY, theme.value)
  apply(theme.value)
}
