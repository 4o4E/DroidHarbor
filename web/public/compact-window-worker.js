// A native window name is only reusable inside a related browsing-context
// group. This worker serializes admission across independent same-origin tabs.
let owner = null
let sequence = 0

function create(port, target) {
  const token = `${Date.now().toString(36)}-${++sequence}`
  owner = { token, opener: port, popup: null, target, requester: port }
  port.postMessage({ type: 'create', token, target })
}

onconnect = event => {
  const port = event.ports[0]
  port.onmessage = event => {
    const message = event.data
    if (message.type === 'open') {
      if (!owner) { create(port, message.target); return }
      owner.target = message.target
      owner.requester = port
      if (owner.opener) owner.opener.postMessage({ type: 'focus', token: owner.token })
      if (owner.popup) owner.popup.postMessage({ type: 'show', target: owner.target })
    } else if (message.type === 'register-popup') {
      if (!owner || owner.token !== message.token || (owner.popup && owner.popup !== port)) { port.postMessage({ type: 'close-duplicate' }); return }
      owner.popup = port
      port.postMessage({ type: 'show', target: owner.target })
    } else if (message.type === 'closed' && owner && owner.token === message.token && owner.popup === port) {
      owner = null
    } else if (message.type === 'creation-failed' && owner && owner.token === message.token && owner.opener === port) {
      owner = null
    } else if (message.type === 'gone' && owner && owner.token === message.token && owner.opener === port) {
      const next = owner.requester
      const target = owner.target
      owner = null
      create(next, target)
    } else if (message.type === 'leave-parent' && owner && owner.opener === port) {
      owner.opener = null
    }
  }
  port.start()
}
