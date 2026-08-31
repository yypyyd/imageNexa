import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// Dev server proxies backend routes to the Go backend (default :6666) so the
// frontend can use relative paths exactly like the old static UI did. Override
// the target with VITE_BACKEND when the backend runs elsewhere.
const backend = process.env.VITE_BACKEND || 'http://127.0.0.1:6666'

// Vite's underlying http-proxy doesn't add X-Forwarded-For / X-Real-IP by
// default, so the backend just sees the proxy's loopback address as the
// caller and stamps every login as 127.0.0.1. This hook forwards the real
// socket peer instead. (Only useful when the dev server is reachable from
// another device on the LAN — same-machine browsing is genuinely 127.0.0.1.)
function forwardClientIp(proxy) {
  proxy.on('proxyReq', (proxyReq, req) => {
    const ip = (req.socket && req.socket.remoteAddress) || ''
    if (!ip) return
    const existing = req.headers['x-forwarded-for']
    proxyReq.setHeader('x-forwarded-for', existing ? `${existing}, ${ip}` : ip)
    if (!req.headers['x-real-ip']) proxyReq.setHeader('x-real-ip', ip)
  })
}

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: {
    port: 5173,
    proxy: {
      // Only the admin API is proxied — bare /admin/* is an SPA route now
      // (the admin shell), handled client-side by vue-router.
      // Regex keeps the SPA route /admin/api-keys on the frontend while all
      // real control-plane calls (/admin/api/...) still reach the backend.
      '^/admin/api/': { target: backend, changeOrigin: true, configure: forwardClientIp },
      '/health': { target: backend, changeOrigin: true, configure: forwardClientIp },
      // Generated artifacts are served from /images.
      '/images': { target: backend, changeOrigin: true, configure: forwardClientIp },
      '/v1': { target: backend, changeOrigin: true, configure: forwardClientIp },
    },
  },
})
