import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'

export default {
  // The bundle is served by Go under a random mount path, so asset URLs stay
  // relative to the page that loads them.
  base: './',
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    // In development Vite serves the app at / and forwards /api to the Go
    // process, rewritten onto the panel's real path. Point QGB_PANEL_PATH at
    // the path the server printed on startup.
    proxy: process.env.QGB_PANEL_PATH && {
      '/api': {
        target: 'http://127.0.0.1:8092',
        changeOrigin: false,
        rewrite: (p) => process.env.QGB_PANEL_PATH + p,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
}
