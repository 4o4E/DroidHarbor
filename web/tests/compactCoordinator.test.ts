import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { createContext, runInContext } from 'node:vm'

function coordinator() {
  const context = createContext({ onconnect: undefined })
  runInContext(readFileSync(new URL('../public/compact-window-worker.js', import.meta.url), 'utf8'), context)
  const connect = () => {
    const messages: { type: string; token?: string; target?: unknown }[] = []
    const port = { onmessage: null as null | ((event: { data: unknown }) => void), postMessage: (message: typeof messages[number]) => messages.push(message), start() {} }
    context.onconnect({ ports: [port] })
    return { messages, send: (data: unknown) => port.onmessage!({ data }) }
  }
  return connect
}

test('closing a rejected duplicate does not forget the existing popup', () => {
  const connect = coordinator()
  const parent = connect()
  parent.send({ type: 'open', target: { deviceId: 'a' } })
  const token = parent.messages[0].token
  const original = connect()
  original.send({ type: 'register-popup', token })
  const duplicate = connect()
  duplicate.send({ type: 'register-popup', token })
  assert.equal(duplicate.messages.at(-1)?.type, 'close-duplicate')
  duplicate.send({ type: 'closed', token })
  const secondParent = connect()
  secondParent.send({ type: 'open', target: { deviceId: 'b' } })
  assert.equal(secondParent.messages.some(message => message.type === 'create'), false)
  assert.deepEqual(original.messages.at(-1)?.target, { deviceId: 'b' })
  original.send({ type: 'closed', token })
  secondParent.send({ type: 'open', target: { deviceId: 'c' } })
  assert.equal(secondParent.messages.at(-1)?.type, 'create')
})
