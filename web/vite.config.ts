import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: 'dist', emptyOutDir: true },
  server: {
    proxy: {
      // Deliberately changeOrigin: false, and deliberately spelled out
      // rather than using the '/api': 'target' shorthand. requireSameOrigin
      // has two paths: Sec-Fetch-Site (checked first) and, only when that
      // header is absent, an Origin-vs-Host fallback for older clients.
      // Sec-Fetch-Site is computed by the browser against the page's own
      // origin (:5173) before the proxy is ever involved, so it already
      // reads "same-origin" and Vite forwards it unchanged — that path is
      // unaffected by this setting either way. The fallback compares the
      // Origin header's host against the request Host; changeOrigin: false
      // preserves the original Host (:5173) the browser sent, matching
      // Origin (also :5173). changeOrigin: true rewrites Host to the proxy
      // target (:8080) while leaving Origin at :5173, so they stop matching
      // and the fallback starts rejecting every request it covers.
      //
      // Verified empirically against the running server, logged in, POSTing
      // /api/runs with only an Origin header (no Sec-Fetch-Site) through
      // this proxy: changeOrigin: true and the '/api': 'target' shorthand
      // (which defaults changeOrigin to true) both came back 403; the
      // explicit changeOrigin: false below came back 202, reproduced twice.
      '/api': { target: 'http://localhost:8080', changeOrigin: false },
    },
  },
  test: { environment: 'jsdom', globals: true },
})
