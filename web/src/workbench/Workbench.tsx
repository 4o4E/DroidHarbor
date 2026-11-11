import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { Layout } from 'react-resizable-panels'
import { Circle, CircleDot, ExternalLink, LoaderCircle, ShieldAlert, Unplug, Video, Wrench } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import { useMediaSession } from '@/media/useMediaSession'
import { deviceDisplaySize, deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { WorkbenchTools } from './WorkbenchTools'
import { PhoneView } from './PhoneView'
import { useClipboardHistory } from './ClipboardHistory'
import { compactWindowSize } from './compactWindowSize'

type Props = { device: Device; devices: readonly Device[]; onDeviceChange: (device: Device) => void; mode: SessionMode; onModeChange: (mode: SessionMode) => void }

export function Workbench({ device, devices, onDeviceChange, mode, onModeChange }: Props) {
  const [layout, setLayout] = useState<Layout>()
  const [sessionAllowed, setSessionAllowed] = useState({ deviceId: device.deviceId, allowed: true })
  const connection = useRef({ deviceId: device.deviceId, hasBeenOnline: device.connectionState === 'online' })
  const activeDevice = useRef(device.deviceId)
  activeDevice.current = device.deviceId
  const reconnectTimer = useRef<number | undefined>(undefined)
  const videoAreaRef = useRef<HTMLDivElement | null>(null)
  const [videoAreaSize, setVideoAreaSize] = useState({ width: 0, height: 0 })
  const history = useClipboardHistory(device.deviceId)
  // A disconnected live session needs an explicit reconnect; selecting another
  // device may start that device without restoring the disconnected session.
  useEffect(() => {
    if (connection.current.deviceId !== device.deviceId) {
      connection.current = { deviceId: device.deviceId, hasBeenOnline: device.connectionState === 'online' }
      setSessionAllowed({ deviceId: device.deviceId, allowed: true })
    } else if (device.connectionState === 'online') connection.current.hasBeenOnline = true
    else if (connection.current.hasBeenOnline) setSessionAllowed({ deviceId: device.deviceId, allowed: false })
  }, [device.deviceId, device.connectionState])
  useEffect(() => () => window.clearTimeout(reconnectTimer.current), [])
  const allowed = sessionAllowed.deviceId !== device.deviceId || sessionAllowed.allowed
  const session = useMediaSession(device.deviceId, mode, device.connectionState === 'online' && allowed, history.record)
  useLayoutEffect(() => {
    const area = videoAreaRef.current
    if (!area) return
    const measure = () => {
      const { width, height } = area.getBoundingClientRect()
      setVideoAreaSize(current => current.width === width && current.height === height ? current : { width, height })
    }
    measure()
    const observer = new ResizeObserver(measure)
    observer.observe(area)
    return () => observer.disconnect()
  }, [])
  const reconnect = () => {
    window.clearTimeout(reconnectTimer.current)
    const deviceId = device.deviceId
    setSessionAllowed({ deviceId, allowed: false })
    reconnectTimer.current = window.setTimeout(() => {
      if (activeDevice.current === deviceId) setSessionAllowed({ deviceId, allowed: true })
    }, 0)
  }
  const savedSize = deviceDisplaySize(device)
  const displaySize = session.width && session.height ? { width: session.width, height: session.height } : savedSize
  const openCompact = () => {
    if (!displaySize) return
    const url = new URL(window.location.href)
    url.search = new URLSearchParams({ view: 'compact', device: device.deviceId, mode }).toString()
    const size = compactWindowSize(displaySize.width / displaySize.height, 0.8)
    const popup = window.open('about:blank', '_blank', `popup,width=${size.innerWidth},height=${size.innerHeight}`)
    if (!popup) {
      toast.add({ title: '浏览器阻止了小窗', description: '允许此网站打开弹窗后，再点击“独立小窗”。', type: 'error', timeout: 0, actionProps: { children: '重新打开', onClick: openCompact } })
      return
    }
    popup.opener = null
    popup.location.replace(url)
  }
  const stateLabel = device.connectionState !== 'online' ? deviceStatus(device) : session.status === 'failed' ? '视频连接失败' : session.status === 'closed' ? '视频已关闭' : session.ready ? '画面已就绪' : session.status === 'connected' ? '等待画面' : '正在连接视频'
  const sortedDevices = [...devices].sort((a, b) => Number(a.connectionState !== 'online') - Number(b.connectionState !== 'online'))
  return <div className="workbench-layout">
    <ResizablePanelGroup orientation="horizontal" defaultLayout={layout} onLayoutChanged={(next, meta) => { if (meta.isUserInteraction) setLayout(meta.requestedLayout ?? next) }}>
      <ResizablePanel id="devices" defaultSize="224px" minSize="180px" maxSize="440px" className="h-full">
        <aside className="device-sidebar" aria-label="设备预览列表">
          <div className="device-sidebar-header"><ToggleGroup spacing={0} variant="outline" value={[mode]} onValueChange={values => { if (values[0] === 'readonly' || values[0] === 'control') onModeChange(values[0]) }} aria-label="会话模式"><ToggleGroupItem value="readonly">只读</ToggleGroupItem><ToggleGroupItem value="control">控制</ToggleGroupItem></ToggleGroup><Button variant="ghost" size="icon-sm" aria-label="独立小窗" title="独立小窗" disabled={!displaySize} onClick={openCompact}><ExternalLink /></Button></div>
          <ToggleGroup orientation="vertical" variant="outline" className="device-list" value={[device.deviceId]} onValueChange={values => { const next = devices.find(item => item.deviceId === values[0]); if (next) onDeviceChange(next) }} aria-label="切换设备">
            {sortedDevices.map(item => <ToggleGroupItem key={item.deviceId} value={item.deviceId} aria-label={`切换到${deviceName(item)}`} className={`device-list-item ${item.connectionState !== 'online' ? 'device-unavailable' : ''}`}>
              <DevicePreview device={item} compact />
              <span className="device-list-info"><span className="device-list-name">{deviceName(item)}</span><span className="device-occupancy" role="img" aria-label={item.allocationState === 'leased' ? '已分配' : '未分配'} title={item.allocationState === 'leased' ? '已分配给 AI 任务' : '未分配给 AI 任务'}>{item.allocationState === 'leased' ? <CircleDot /> : <Circle />}</span>
                <span className="device-exceptions">
                  {deviceNeedsVerification(item) ? <ShieldAlert aria-label="待核验"><title>待核验</title></ShieldAlert> : item.connectionState !== 'online' ? <Unplug aria-label="设备不可用"><title>{deviceStatus(item)}</title></Unplug> : null}
                  {item.allocationState === 'maintenance' && <Wrench aria-label="维护中"><title>维护中</title></Wrench>}
                  {item.allocationState === 'quarantined' && <ShieldAlert aria-label="已隔离"><title>已隔离</title></ShieldAlert>}
                  {item.allocationState === 'reclaiming' && <LoaderCircle aria-label="回收中"><title>回收中</title></LoaderCircle>}
                </span>
                {item.deviceId === device.deviceId && <span className={`device-video-state ${session.status === 'failed' ? 'text-destructive' : ''}`}><span className="flex items-center gap-1" title={`当前页面：${stateLabel}`}><Video />{stateLabel}</span>{session.width > 0 && <span>{session.width} × {session.height}<br />{session.framesPerSecond.toFixed(1)} fps</span>}</span>}
              </span>
            </ToggleGroupItem>)}
          </ToggleGroup>
        </aside>
      </ResizablePanel>
      <ResizableHandle withHandle aria-label="调整设备侧栏宽度" />
      <ResizablePanel id="screen" minSize="240px" className="h-full">
        <section ref={videoAreaRef} className="phone-stage" aria-label="设备画面"><PhoneView key={device.deviceId} device={device} mode={mode} session={session} availableArea={videoAreaSize} stateLabel={stateLabel} onReconnect={reconnect} /></section>
      </ResizablePanel>
    </ResizablePanelGroup>
    <WorkbenchTools key={device.deviceId} device={device} mode={mode} session={session} />
  </div>
}
