import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'

export default {
  // The bundle is served by Go under /admin/, so every asset URL has to carry it.
  base: '/admin/',
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    // In development Vite serves the app and forwards the API to the Go process,
    // so the cookie session stays first-party and no CORS widening is needed.
    proxy: {
      '/admin/api': { target: 'http://127.0.0.1:8092', changeOrigin: false },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
}
