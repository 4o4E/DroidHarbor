import { cp, mkdir, readFile, rm, writeFile, access } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const root = fileURLToPath(new URL('../', import.meta.url))
const source = path.join(root, 'web/dist')
const target = path.join(root, 'internal/bundle')
const server = await readFile(path.join(root, '.tools/scrcpy-server-v5.0.1'))
const digest = createHash('sha256').update(server).digest('hex')
if (server.length !== 733930 || digest !== '764eb6f79811d5211fe9df341120882ba9994c7a61b897d7bf3fb662e53bc536') {
  throw new Error('Pinned scrcpy-server v5.0.1 size or SHA-256 mismatch')
}
await access(path.join(source, 'index.html'))
await mkdir(target, { recursive: true })
await rm(path.join(target, 'frontend'), { recursive: true, force: true })
await cp(source, path.join(target, 'frontend'), { recursive: true })
await writeFile(path.join(target, 'scrcpy-server-v5.0.1'), server)
console.log('Prepared embedded frontend and verified scrcpy-server v5.0.1')
