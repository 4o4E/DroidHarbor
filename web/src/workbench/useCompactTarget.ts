import { useEffect, useState } from 'react'
import { compactCoordinator, type CompactTarget } from './openCompact'

export function useCompactTarget(deviceId: string | null, mode: string | null) {
 const [target, setTarget] = useState({ deviceId, mode })
 useEffect(() => {
  const token = new URLSearchParams(location.search).get('windowToken')
  if (!token) return
  const port = compactCoordinator()
  if (!port) return
  const receive = (event: MessageEvent<{ type: string; target: CompactTarget }>) => {
   if (event.data.type === 'close-duplicate') { window.close(); return }
   if (event.data.type !== 'show') return
   const next = event.data.target
   const url = new URL(location.href)
   url.searchParams.set('device', next.deviceId)
   url.searchParams.set('mode', next.mode)
   history.replaceState(null, '', url)
   setTarget({ deviceId: next.deviceId, mode: next.mode })
   window.focus()
  }
  port.addEventListener('message', receive)
  port.postMessage({ type: 'register-popup', token })
  return () => { port.removeEventListener('message', receive) }
 }, [])
 return target
}
