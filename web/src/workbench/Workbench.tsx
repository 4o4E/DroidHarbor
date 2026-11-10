import { useEffect, useRef, useState } from 'react'
import type { Layout } from 'react-resizable-panels'
import { Camera, ChevronDown, ChevronLeft, Circle, CircleDot, ExternalLink, FlipHorizontal2, LoaderCircle, Monitor, Pause, Play, Power, RefreshCw, RotateCcw, RotateCw, ShieldAlert, Square, Unplug, Video, Volume2, Wrench } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import { VideoSurface } from '@/media/VideoSurface'
import { useMediaSession } from '@/media/useMediaSession'
import { deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { WorkbenchTools } from './WorkbenchTools'
import { useClipboardHistory } from './ClipboardHistory'
import { compactWindowSize, PHONE_TOOLBAR_WIDTH } from './compactWindowSize'
import { downloadScreenshot } from '@/lib/screenshots'

type Props = { device: Device; devices: readonly Device[]; onDeviceChange: (device: Device) => void; mode: SessionMode; onModeChange: (mode: SessionMode) => void }
type LayoutProps = { layout: Layout | undefined; onLayoutChanged: (layout: Layout) => void }
export function Workbench(props: Props) {
  const [layout, setLayout] = useState<Layout>()
  return <WorkbenchSession key={props.device.deviceId} {...props} layout={layout} onLayoutChanged={setLayout} />
}
function WorkbenchSession({ device, devices, onDeviceChange, mode, onModeChange, layout, onLayoutChanged }: Props & LayoutProps) {
  const [sessionAllowed, setSessionAllowed] = useState(true)
  const hasBeenOnline = useRef(device.connectionState === 'online')
  const needsVerification = deviceNeedsVerification(device)
  const [lastPowerCommand, setLastPowerCommand] = useState<boolean | null>(null)
  const [displayRotation, setDisplayRotation] = useState(0)
  const [mirrored, setMirrored] = useState(false)
  const [paused, setPaused] = useState(false)
  const [actionBusy, setActionBusy] = useState(false)
  const [rotatingPhone, setRotatingPhone] = useState(false)
  const phoneRotation = useRef<{ landscape: boolean; timer?: number; observed: boolean; cancelled: boolean } | null>(null)
  const videoAreaRef = useRef<HTMLDivElement | null>(null)
  const [videoAreaSize, setVideoAreaSize] = useState({ width: 0, height: 0 })
  const history = useClipboardHistory(device.deviceId)
  // A disconnected live session needs an explicit reconnect; a first successful
  // identity check may start the initial session without restoring an old one.
  useEffect(() => {
    if (device.connectionState === 'online') hasBeenOnline.current = true
    else if (hasBeenOnline.current) setSessionAllowed(false)
  }, [device.connectionState])
  const session = useMediaSession(device.deviceId, mode, device.connectionState === 'online' && sessionAllowed, history.record)
  const canControl = mode === 'control' && session.status === 'connected' && session.ready && !!session.sessionId
  const rotated = Math.round(displayRotation / 90) % 2 !== 0
  const videoRatio = session.width > 0 && session.height > 0 ? rotated ? session.height / session.width : session.width / session.height : 1
  const frameWidth = Math.min(Math.max(0, videoAreaSize.width - PHONE_TOOLBAR_WIDTH), videoAreaSize.height * videoRatio)
  const frameHeight = frameWidth / videoRatio
  useEffect(() => {
    const pending = phoneRotation.current
    if (pending && session.ready && session.width > 0 && session.height > 0 && (session.width > session.height) !== pending.landscape) {
      pending.observed = true
      window.clearTimeout(pending.timer)
      phoneRotation.current = null
      setRotatingPhone(false)
    }
  }, [session.ready, session.width, session.height])
  useEffect(() => {
    setRotatingPhone(false)
    return () => {
      const pending = phoneRotation.current
      if (pending) { pending.cancelled = true; window.clearTimeout(pending.timer) }
      phoneRotation.current = null
    }
  }, [mode])
  useEffect(() => {
    const area = videoAreaRef.current
    if (!area) return
    const observer = new ResizeObserver(entries => {
      const { width, height } = entries[0].contentRect
      setVideoAreaSize(current => current.width === width && current.height === height ? current : { width, height })
    })
    observer.observe(area)
    return () => observer.disconnect()
  }, [])
  const openCompact = () => {
    const url = new URL(window.location.href)
    url.search = new URLSearchParams({ view: 'compact', device: device.deviceId, mode }).toString()
    const size = compactWindowSize(session.width / session.height, 0.8)
    const popup = window.open('about:blank', '_blank', `popup,width=${size.innerWidth},height=${size.innerHeight}`)
    if (!popup) {
      toast.add({ title: '浏览器阻止了小窗', description: '允许此网站打开弹窗后，再点击“独立小窗”。', type: 'error', timeout: 0, actionProps: { children: '重新打开', onClick: openCompact } })
      return
    }
    popup.opener = null
    popup.location.replace(url)
  }
  const runCommand = async (type: string, payload: Record<string, unknown> = {}) => {
    if (!canControl || actionBusy) return false
    setActionBusy(true)
    try {
      const reply = await session.command(type, payload)
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(reply.status)) throw new Error(reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`)
      return true
    } catch (error) { toast.add({ title: '设备操作未完成', description: errorMessage(error), type: 'error', timeout: 0 }); return false }
    finally { setActionBusy(false) }
  }
  const pressVolume = async (keyCode: number) => {
    if (!canControl || actionBusy) return
    setActionBusy(true)
    try {
      const down = await session.command('key', { keyCode, action: 0 })
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(down.status)) throw new Error(down.message || down.code || `设备拒绝了按键（${down.status}）`)
      const up = await session.command('key', { keyCode, action: 1 })
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(up.status)) throw new Error(up.message || up.code || `设备拒绝了按键（${up.status}）`)
    } catch (error) { session.release(); toast.add({ title: '按键操作未完成', description: errorMessage(error), type: 'error', timeout: 0 }) }
    finally { setActionBusy(false) }
  }
  const takeScreenshot = async () => {
    if (!session.sessionId) return
    try { await downloadScreenshot(session.sessionId) }
    catch (error) { toast.add({ title: '截图失败', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }
  const rotatePhone = async () => {
    if (!canControl || actionBusy || rotatingPhone) return
    const pending = { landscape: session.width > session.height, timer: undefined as number | undefined, observed: false, cancelled: false }
    phoneRotation.current = pending
    setRotatingPhone(true)
    setActionBusy(true)
    try {
      const reply = await session.command('rotate', {})
      if (!['succeeded', 'dispatched'].includes(reply.status)) throw new Error(reply.message || reply.code || `旋转请求结果：${reply.status}`)
      if (phoneRotation.current === pending) pending.timer = window.setTimeout(() => {
        phoneRotation.current = null
        setRotatingPhone(false)
        toast.add({ title: '手机方向未改变', description: '当前应用或系统可能限制旋转。仅调整预览可使用“旋转显示”。', type: 'warning', timeout: 0 })
      }, 5000)
    } catch (error) {
      if (pending.cancelled) return
      window.clearTimeout(pending.timer)
      phoneRotation.current = null
      setRotatingPhone(false)
      toast.add({ title: pending.observed ? '方向已切换，命令响应未确认' : '设备旋转未完成', description: errorMessage(error), type: pending.observed ? 'warning' : 'error', timeout: 0 })
    } finally { setActionBusy(false) }
  }
  const stateLabel = device.connectionState !== 'online' ? deviceStatus(device) : session.status === 'failed' ? '视频连接失败' : session.status === 'closed' ? '视频已关闭' : session.ready ? '画面已就绪' : session.status === 'connected' ? '等待画面' : '正在连接视频'
  const sortedDevices = [...devices].sort((a, b) => Number(a.connectionState !== 'online') - Number(b.connectionState !== 'online'))
  return <div className="workbench-layout">
    <ResizablePanelGroup orientation="horizontal" defaultLayout={layout} onLayoutChanged={(next, meta) => { if (meta.isUserInteraction) onLayoutChanged(meta.requestedLayout ?? next) }}>
      <ResizablePanel id="devices" defaultSize="224px" minSize="180px" maxSize="440px" className="h-full">
        <aside className="device-sidebar" aria-label="设备预览列表">
          <div className="device-sidebar-header"><ToggleGroup spacing={0} variant="outline" value={[mode]} onValueChange={values => { if (values[0] === 'readonly' || values[0] === 'control') onModeChange(values[0]) }} aria-label="会话模式"><ToggleGroupItem value="readonly">只读</ToggleGroupItem><ToggleGroupItem value="control">控制</ToggleGroupItem></ToggleGroup><Button variant="ghost" size="icon-sm" aria-label="独立小窗" title="独立小窗" disabled={!session.width || !session.height} onClick={openCompact}><ExternalLink /></Button></div>
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
        <section ref={videoAreaRef} className="phone-stage" aria-label="设备画面">
          <div className="phone-arrangement" style={{ width: frameWidth + PHONE_TOOLBAR_WIDTH, height: frameHeight }}>
            <div className="phone-toolbar" style={{ width: PHONE_TOOLBAR_WIDTH }} aria-label="设备快捷操作">
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label={lastPowerCommand === false ? '唤醒屏幕' : '息屏'} title={lastPowerCommand === false ? '唤醒屏幕' : '息屏'} onClick={() => { const on = lastPowerCommand === false; void runCommand('displayPower', { on }).then(sent => { if (sent) setLastPowerCommand(on) }) }}><Power /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="音量增加" title="音量增加" onClick={() => void pressVolume(24)}><Volume2 /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="音量降低" title="音量降低" onClick={() => void pressVolume(25)}><Volume2 className="rotate-180" /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy || rotatingPhone} aria-label="旋转手机" title="旋转手机" onClick={() => void rotatePhone()}><RotateCw /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!session.ready} aria-label="旋转显示" title="旋转显示（不改变手机方向）" onClick={() => setDisplayRotation(current => (current + 90) % 360)}><RotateCcw /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!session.ready} aria-label={mirrored ? '取消镜像显示' : '镜像显示'} aria-pressed={mirrored} title="镜像显示" onClick={() => setMirrored(current => !current)}><FlipHorizontal2 /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!session.ready} aria-label={paused ? '继续画面' : '暂停画面'} aria-pressed={paused} title={paused ? '继续画面' : '暂停画面'} onClick={() => setPaused(current => !current)}>{paused ? <Play /> : <Pause />}</Button>
              <Button size="icon-sm" variant="ghost" disabled={!session.sessionId} aria-label="截图" title="截图" onClick={() => void takeScreenshot()}><Camera /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="通知面板" title="通知面板" onClick={() => void runCommand('notification')}><Unplug /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="系统设置" title="系统设置" onClick={() => void runCommand('settings')}><Monitor /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="收起通知面板" title="收起通知面板" onClick={() => void runCommand('collapse')}><ChevronDown /></Button>
              <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="重置视频" title="重置视频" onClick={() => void runCommand('resetVideo')}><RefreshCw /></Button>
              <Button size="icon-sm" variant="ghost" disabled={device.connectionState !== 'online'} aria-label="重新连接视频" title="重新连接视频" onClick={() => { setSessionAllowed(false); window.setTimeout(() => setSessionAllowed(true), 0) }}><RotateCw /></Button>
            </div>
            <div className="phone-frame" style={{ width: frameWidth, height: frameHeight }}><VideoSurface session={session} mode={mode} rotation={displayRotation} mirrored={mirrored} paused={paused} className="size-full" />{!session.ready && <div className="phone-placeholder"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState !== 'online' ? <Unplug /> : <Monitor />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState !== 'online' ? '设备当前不可用' : session.status === 'failed' ? '视频连接失败' : '等待设备画面'}</EmptyTitle><EmptyDescription>{needsVerification ? '在设备管理中核验身份后即可投屏。' : session.error || stateLabel}</EmptyDescription></EmptyHeader></Empty></div>}</div>
          </div>
        </section>
      </ResizablePanel>
    </ResizablePanelGroup>
    <WorkbenchTools device={device} mode={mode} session={session} />
  </div>
}
