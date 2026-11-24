/// <reference types="vitest/config" />
import react from '@vitejs/plugin-react'
import { mkdirSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { defineConfig, type Plugin } from 'vite'

// The built page only loads its own files: no network fonts, no remote scripts.
// Dev (the mock) skips this so Vite's inline preamble can run.
const csp = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; connect-src 'self'; script-src 'self'"

function desktopBuild(): Plugin {
  return {
    name: 'olsyn-desktop-build',
    apply: 'build',
    transformIndexHtml(html) {
      return html.replace('<meta charset="UTF-8" />', '<meta charset="UTF-8" />\n    <meta http-equiv="Content-Security-Policy" content="' + csp + '" />')
    },
    // ui/assets.go embeds dist/; keep the folder (and its .gitkeep) after Vite empties it.
    closeBundle() {
      const dist = resolve(__dirname, 'dist')
      mkdirSync(dist, { recursive: true })
      writeFileSync(resolve(dist, '.gitkeep'), '')
    },
  }
}

export default defineConfig({
  plugins: [react(), desktopBuild()],
  base: './',
  build: {
    target: 'es2022',
    outDir: 'dist',
    emptyOutDir: true,
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 400,
  },
  test: {
    environment: 'jsdom',
    globals: false,
    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
    setupFiles: ['src/test/setup.ts'],
    restoreMocks: true,
  },
})
