/// <reference types="vitest" />
import path from "path"
import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

const BACKEND = 'http://127.0.0.1:8888'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  test: {
    globals: true,
    environment: 'jsdom',
  },
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes('/node_modules/react') || id.includes('/node_modules/react-dom') || id.includes('/node_modules/react-router-dom')) {
            return 'react'
          }
          if (id.includes('/node_modules/recharts')) {
            return 'charts'
          }
          if (id.includes('/node_modules/@radix-ui')) {
            return 'radix'
          }
          if (id.includes('/node_modules/@stripe')) {
            return 'payments'
          }
          if (id.includes('/node_modules/zod')) {
            return 'validation'
          }
          if (id.includes('/node_modules/react-hook-form')) {
            return 'forms'
          }
          if (id.includes('/node_modules/lucide-react')) {
            return 'icons'
          }
          return undefined
        },
      },
    },
  },
  server: {
    proxy: {
      // ── CPA Gateway API ──
      '/api': {
        target: BACKEND,
        changeOrigin: true,
      },
      // ── CLIProxyAPI SDK 管理 API ──
      '/v0': {
        target: BACKEND,
        changeOrigin: true,
      },
      // ── CLIProxyAPI SDK 代理路由 ──
      '/v1': {
        target: BACKEND,
        changeOrigin: true,
      },
      // ── CLIProxyAPI SDK Gemini 路由 ──
      '/v1beta': {
        target: BACKEND,
        changeOrigin: true,
      },
      // ── 健康检查 ──
      '/healthz': {
        target: BACKEND,
        changeOrigin: true,
      },
      // ── SDK OAuth 浏览器回调 ──
      // 使用正则匹配所有 /{provider}/callback 路径，
      // 避免逐条列举每个 OAuth 提供商。
      '^/(anthropic|codex|google|gemini|iflow|antigravity|qwen|kimi)/callback': {
        target: BACKEND,
        changeOrigin: true,
      },
    }
  }
})
