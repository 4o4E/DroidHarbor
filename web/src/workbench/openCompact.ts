import { toast } from '@/components/ui/toast'
import { deviceDisplaySize, type Device, type SessionMode, type SystemPreferences } from '@/lib/types'
import { compactWindowSize } from './compactWindowSize'

export type CompactTarget = { deviceId: string; mode: SessionMode; width: number; height: number; scale: number }
let compact: Window | null = null
let port: MessagePort | null = null

export function compactCoordinator() {
 if (port) return port
 try {
  const worker = new SharedWorker('/compact-window-worker.js', { name: 'DroidHarborPreviewCoordinator' })
  port = worker.port
  const connection = port
  connection.onmessage = (event: MessageEvent<{ type: string; token: string; target: CompactTarget }>) => {
   const message = event.data
   if (message.type === 'create') {
    const target = message.target
    const url = new URL(location.href)
    url.search = new URLSearchParams({ view: 'compact', device: target.deviceId, mode: target.mode, windowToken: message.token }).toString()
    const size = compactWindowSize(target.width, target.height, target.scale)
    compact = window.open(url, `DroidHarborPreview-${message.token}`, `popup,width=${size.innerWidth},height=${size.innerHeight}`)
    if (!compact) {
     connection.postMessage({ type: 'creation-failed', token: message.token })
     toast.add({ title: '浏览器阻止了小窗', description: '允许此网站打开弹窗后重试。', type: 'error', timeout: 0 })
    } else compact.focus()
   } else if (message.type === 'focus') {
    if (compact && !compact.closed) compact.focus()
    else connection.postMessage({ type: 'gone', token: message.token })
   }
  }
  worker.onerror = () => toast.add({ title: '小窗协调失败', description: '请刷新此页面后重试。', type: 'error', timeout: 0 })
  connection.onmessageerror = () => toast.add({ title: '小窗消息无效', description: '请刷新此页面后重试。', type: 'error', timeout: 0 })
  connection.start()
  window.addEventListener('pagehide', () => { const token = new URLSearchParams(location.search).get('windowToken'); connection.postMessage(token ? { type: 'closed', token } : { type: 'leave-parent' }); connection.close(); port = null }, { once: true })
  return connection
 } catch (error) {
  toast.add({ title: '无法创建独立小窗', description: error instanceof Error ? error.message : String(error), type: 'error', timeout: 0 })
  return null
 }
}

export function openCompact(device: Device, mode: SessionMode, preferences: SystemPreferences) {
 const saved = deviceDisplaySize(device)
 if (!saved) { toast.add({ title: '无法打开小窗', description: '尚未读取到设备屏幕尺寸，请刷新设备状态。', type: 'warning' }); return }
 compactCoordinator()?.postMessage({ type: 'open', target: { deviceId: device.deviceId, mode, width: saved.width, height: saved.height, scale: preferences.windowScales[0] } satisfies CompactTarget })
}
