import { createServer, build } from '../../frontend/node_modules/vite/dist/node/index.js'
import config from './vite.config.mjs'

const options = { ...config, configFile: false }

if (process.argv.includes('--build')) await build(options)
else {
  const server = await createServer(options)
  await server.listen()
  server.printUrls()
}
