// Reactive draft of the 画图 form fields. Lives at module scope so the
// values survive PlaygroundView being unmounted (navigation to 首页/记录 etc.)
// and remounted — without this, switching away and back wiped the prompt
// and selected model. Per-tab only; not persisted to localStorage.
import { reactive } from 'vue'

export const draft = reactive({
  mode: '',           // 'image' | 'video'
  modelId: '',
  prompt: '',
  ratio: '',
  resolution: '',
  duration: '',
})
