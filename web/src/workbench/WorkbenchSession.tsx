import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { useMediaSession } from '@/media/useMediaSession'
import type { MediaSession } from '@/media/types'
import { deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { useClipboardHistory } from './ClipboardHistory'
import { PhoneView } from './PhoneView'
import { useInitializedDevice } from '@/devices/DeviceMetrics'

export type WorkbenchView = { key: string; deviceId: string; mode: SessionMode; instance: number; until: number }
export const workbenchViewKey = (deviceId: string, mode: SessionMode) => `${deviceId}:${mode}`
export function mediaStateLabel(device: Device, session?: MediaSession) {
  if (device.connectionState === 'online' && device.runtime?.powerState !== 'active' && !session?.sessionId) return device.runtime?.powerState === 'failed' ? '设备保活异常' : '正在初始化设备状态'
  return device.connectionState !== 'online' ? deviceStatus(device) : session?.status === 'recovering' ? '正在恢复视频' : session?.status === 'failed' ? '视频连接失败' : session?.status === 'closed' ? '视频已关闭' : session?.ready ? '画面已就绪' : session?.status === 'connected' ? '等待画面' : '正在连接视频'
}

type Props = {
  view: WorkbenchView
  device: Device
  selected: boolean
  after?: Promise<void>
  availableArea: { width: number; height: number }
  onSession: (key: string, session: MediaSession | null) => void
}

// A retained view keeps the same video element, MediaStream and decoder. Only
// input authority and visibility change; sidebar canvases have their own freeze.
export function WorkbenchSession({ view, device, selected, after, availableArea, onSession }: Props) {
  const [allowed, setAllowed] = useState(true)
  const reconnectTimer = useRef<number | undefined>(undefined)
  const history = useClipboardHistory(device.deviceId)
  const initialized = useInitializedDevice(device)
  useEffect(() => () => window.clearTimeout(reconnectTimer.current), [])
  const session = useMediaSession(device.deviceId, view.mode, initialized && allowed, history.record, { active: selected, after })
  useLayoutEffect(() => { onSession(view.key, session) }, [onSession, session, view.key])
  useLayoutEffect(() => () => onSession(view.key, null), [onSession, view.key])
  const reconnect = () => {
    window.clearTimeout(reconnectTimer.current)
    setAllowed(false)
    reconnectTimer.current = window.setTimeout(() => setAllowed(true), 0)
  }
  return <div className={`absolute inset-0 flex items-center justify-center ${selected ? '' : 'invisible pointer-events-none'}`} aria-hidden={!selected} inert={!selected} data-media-device={device.deviceId}>
    <PhoneView device={device} mode={selected ? view.mode : 'readonly'} session={session} availableArea={availableArea} stateLabel={mediaStateLabel(device, session)} onReconnect={reconnect} />
  </div>
}
