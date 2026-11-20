import { createServer, build } from '../../frontend/node_modules/vite/dist/node/index.js'
import { fileURLToPath } from 'node:url'

const options = { configFile: fileURLToPath(new URL('./vite.config.mjs', import.meta.url)) }

if (process.argv.includes('--build')) await build(options)
else {
  const server = await createServer(options)
  await server.listen()
  server.printUrls()
}
