import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { Layout } from 'react-resizable-panels'
import { Circle, CircleDot, ExternalLink, LoaderCircle, ShieldAlert, Unplug, Video, Wrench } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import type { MediaSession } from '@/media/types'
import { deviceDisplaySize, deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { WorkbenchTools } from './WorkbenchTools'
import { WorkbenchSession, mediaStateLabel, workbenchViewKey, type WorkbenchView } from './WorkbenchSession'
import { errorMessage } from '@/lib/api'
import { compactWindowSize } from './compactWindowSize'

type Props = { device: Device; devices: readonly Device[]; onDeviceChange: (device: Device) => void; mode: SessionMode; onModeChange: (mode: SessionMode) => void }

export function Workbench({ device, devices, onDeviceChange, mode, onModeChange }: Props) {
  const [layout, setLayout] = useState<Layout>()
  const selectedKey = workbenchViewKey(device.deviceId, mode)
  const [views, setViews] = useState<{ selected: string; items: WorkbenchView[] }>({ selected: selectedKey, items: [{ key: selectedKey, deviceId: device.deviceId, mode, instance: 0, until: 0 }] })
  const [sessions, setSessions] = useState<Record<string, MediaSession>>({})
  const switchBarrier = useRef<Promise<void> | undefined>(undefined)
  const registerSession = useCallback((key: string, session: MediaSession | null) => {
    setSessions(current => {
      if (current[key] === session || (!session && !(key in current))) return current
      const next = { ...current }
      if (session) next[key] = session
      else delete next[key]
      return next
    })
  }, [])
  if (views.selected !== selectedKey) {
    const now = performance.now()
    const cached = views.items.find(item => item.key === selectedKey)
    const target = cached && cached.until > now && sessions[selectedKey]?.canRetain
      ? { ...cached, until: 0 }
      : { key: selectedKey, deviceId: device.deviceId, mode, instance: (cached?.instance ?? 0) + 1, until: 0 }
    const retained = views.items
      .filter(item => item.key !== selectedKey && item.mode === mode && sessions[item.key]?.canRetain && (item.key === views.selected || item.until > now))
      .map(item => item.key === views.selected ? { ...item, until: now + 15_000 } : item)
    setViews({ selected: selectedKey, items: [...retained, target].sort((a, b) => a.key.localeCompare(b.key)) })
  }
  const session = sessions[selectedKey]
  const previewCapture = useRef<(() => void) | null>(null)
  const videoAreaRef = useRef<HTMLDivElement | null>(null)
  const [videoAreaSize, setVideoAreaSize] = useState({ width: 0, height: 0 })
  useEffect(() => {
    const until = Math.min(...views.items.filter(item => item.key !== views.selected).map(item => item.until))
    if (!Number.isFinite(until)) return
    const timer = window.setTimeout(() => {
      setViews(current => ({ ...current, items: current.items.filter(item => item.key === current.selected || item.until > performance.now()) }))
    }, Math.max(0, Math.ceil(until - performance.now())))
    return () => window.clearTimeout(timer)
  }, [views])
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
  const selectDevice = (next: Device) => {
    if (next.deviceId === device.deviceId) return
    previewCapture.current?.()
    const handoff = session?.canRetain
      ? session.suspend()
      : session?.close() ?? Promise.resolve()
    // 等待原窗口关闭流程并报告错误；新设备不继承原窗口失败的建流屏障。
    switchBarrier.current = handoff.catch(cause => {
      toast.add({ title: '设备切换未完成', description: errorMessage(cause), type: 'error', timeout: 0 })
    })
    onDeviceChange(next)
  }
  const savedSize = deviceDisplaySize(device)
  const displaySize = session?.width && session.height ? { width: session.width, height: session.height } : savedSize
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
  const stateLabel = mediaStateLabel(device, session)
  const sortedDevices = [...devices].sort((a, b) => Number(a.connectionState !== 'online') - Number(b.connectionState !== 'online'))
  return <div className="workbench-layout">
    <ResizablePanelGroup orientation="horizontal" defaultLayout={layout} onLayoutChanged={(next, meta) => { if (meta.isUserInteraction) setLayout(meta.requestedLayout ?? next) }}>
      <ResizablePanel id="devices" defaultSize="224px" minSize="180px" maxSize="440px" className="h-full">
        <aside className="device-sidebar" aria-label="设备预览列表">
          <div className="device-sidebar-header"><ToggleGroup spacing={0} variant="outline" value={[mode]} onValueChange={values => { if (values[0] === 'readonly' || values[0] === 'control') onModeChange(values[0]) }} aria-label="会话模式"><ToggleGroupItem value="readonly">只读</ToggleGroupItem><ToggleGroupItem value="control">控制</ToggleGroupItem></ToggleGroup><Button variant="ghost" size="icon-sm" aria-label="独立小窗" title="独立小窗" disabled={!displaySize} onClick={openCompact}><ExternalLink /></Button></div>
          <ToggleGroup orientation="vertical" variant="outline" className="device-list" value={[device.deviceId]} onValueChange={values => { const next = devices.find(item => item.deviceId === values[0]); if (next) selectDevice(next) }} aria-label="切换设备">
            {sortedDevices.map(item => {
              const selected = item.deviceId === device.deviceId
              const size = selected ? displaySize : deviceDisplaySize(item)
              return <ToggleGroupItem key={item.deviceId} value={item.deviceId} aria-label={`切换到${deviceName(item)}`} className={`device-list-item ${item.connectionState !== 'online' ? 'device-unavailable' : ''}`}>
              <DevicePreview device={item} compact poll={false} videoRef={selected ? session?.videoRef : undefined} videoReady={selected && !!session?.ready} captureRef={selected ? previewCapture : undefined} />
              <span className="device-list-info"><span className="device-list-name">{deviceName(item)}</span><span className="device-occupancy" role="img" aria-label={item.allocationState === 'leased' ? '已分配' : '未分配'} title={item.allocationState === 'leased' ? '已分配给 AI 任务' : '未分配给 AI 任务'}>{item.allocationState === 'leased' ? <CircleDot /> : <Circle />}</span>
                <span className="device-exceptions">
                  {deviceNeedsVerification(item) ? <ShieldAlert aria-label="待核验"><title>待核验</title></ShieldAlert> : item.connectionState !== 'online' ? <Unplug aria-label="设备不可用"><title>{deviceStatus(item)}</title></Unplug> : null}
                  {item.allocationState === 'maintenance' && <Wrench aria-label="维护中"><title>维护中</title></Wrench>}
                  {item.allocationState === 'quarantined' && <ShieldAlert aria-label="已隔离"><title>已隔离</title></ShieldAlert>}
                  {item.allocationState === 'reclaiming' && <LoaderCircle aria-label="回收中"><title>回收中</title></LoaderCircle>}
                </span>
                <span className={`device-video-state ${selected && session?.status === 'failed' ? 'text-destructive' : ''}`}>{selected && <span className="flex items-center gap-1" title={`当前页面：${stateLabel}`}><Video />{stateLabel}</span>}{size && <span>{size.width} × {size.height}{selected && session?.ready && <><br />{session.framesPerSecond.toFixed(1)} fps</>}</span>}</span>
              </span>
            </ToggleGroupItem>})}
          </ToggleGroup>
        </aside>
      </ResizablePanel>
      <ResizableHandle withHandle aria-label="调整设备侧栏宽度" />
      <ResizablePanel id="screen" minSize="240px" className="h-full">
        <section ref={videoAreaRef} className="phone-stage relative" aria-label="设备画面">{views.items.map(view => {
          const item = devices.find(candidate => candidate.deviceId === view.deviceId)
          return item ? <WorkbenchSession key={`${view.key}:${view.instance}`} view={view} device={item} selected={view.key === selectedKey} after={switchBarrier.current} availableArea={videoAreaSize} onSession={registerSession} /> : null
        })}</section>
      </ResizablePanel>
    </ResizablePanelGroup>
    {session && <WorkbenchTools key={device.deviceId} device={device} mode={mode} session={session} />}
  </div>
}
