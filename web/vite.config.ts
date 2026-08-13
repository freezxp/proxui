import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    // The Go binary embeds this directory and serves it, so one artifact
    // ships both halves of the portal.
    outDir: 'dist',
    sourcemap: false,
  },
  server: {
    port: 5173,
    // In development the SPA runs on Vite and the API on the Go binary;
    // proxying keeps the browser on one origin so cookies and the WebSocket
    // behave exactly as they will in production.
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', changeOrigin: true },
      '/ws': { target: 'ws://127.0.0.1:8080', ws: true },
    },
  },
})
