import vue from '../../frontend/node_modules/@vitejs/plugin-vue/dist/index.mjs'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'

const root = fileURLToPath(new URL('.', import.meta.url))
const repo = resolve(root, '../..')
const snapshot = resolve(repo, '.dev/channel-monitor-design/source-snapshot.json')
export default {
  root,
  envDir: root,
  plugins: [vue(), {
    name: 'local-monitor-snapshot',
    configureServer(server) {
      server.middlewares.use('/__preview/source', async (req, res) => {
        if (req.method !== 'GET') { res.writeHead(405).end(); return }
        res.setHeader('Content-Type', 'application/json; charset=utf-8')
        res.setHeader('Cache-Control', 'no-store')
        try { res.end(await readFile(snapshot, 'utf8')) }
        catch { res.writeHead(404).end(JSON.stringify({ error: '本机尚未导入主站只读快照' })) }
      })
    },
  }],
  resolve: { alias: {
    '@': resolve(repo, 'frontend/src'),
    vue: resolve(repo, 'frontend/node_modules/vue/dist/vue.esm-bundler.js'),
  } },
  server: { host: '127.0.0.1', port: 4180, strictPort: true, fs: { allow: [root, resolve(repo, 'frontend')] } },
  build: { outDir: resolve(root, 'dist'), emptyOutDir: true },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.mjs'],
  },
}
