import { useEffect, useRef, useState } from 'react'
import { ArrowLeft, Camera, ChevronDown, ChevronLeft, Circle, Expand, ExternalLink, FlipHorizontal2, Monitor, Pause, Play, Power, RefreshCw, RotateCcw, RotateCw, Square, Unplug, Volume2 } from 'lucide-react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Separator } from '@/components/ui/separator'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import { VideoSurface } from '@/media/VideoSurface'
import { useMediaSession } from '@/media/useMediaSession'
import { deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { WorkbenchTools } from './WorkbenchTools'
import { downloadScreenshot } from '@/lib/screenshots'

const allocationLabels = { available: '空闲', leased: 'AI 使用中', reclaiming: '回收中', quarantined: '已隔离', maintenance: '维护中' } as const
const sessionStatusLabels = { connecting: '连接中', connected: '已连接', closed: '未连接', failed: '连接失败' } as const
const connectionLabels = { online: '在线', offline: '离线', unauthorized: '未授权' } as const
type WorkbenchProps = {
  device: Device
  devices: readonly Device[]
  onDeviceChange: (device: Device) => void
  mode: SessionMode
  onModeChange: (mode: SessionMode) => void
  onBack?: () => void
}

export function Workbench(props: WorkbenchProps) {
  return <WorkbenchSession key={props.device.deviceId} {...props} />
}

function WorkbenchSession({ device, devices, onDeviceChange, mode, onModeChange, onBack }: WorkbenchProps) {
  const [popupBlocked, setPopupBlocked] = useState(false)
  const [sessionAllowed, setSessionAllowed] = useState(true)
  const hasBeenOnline = useRef(device.connectionState === 'online')
  const needsVerification = deviceNeedsVerification(device)
  const [lastPowerCommand, setLastPowerCommand] = useState<boolean | null>(null)
  const [displayRotation, setDisplayRotation] = useState(0)
  const [mirrored, setMirrored] = useState(false)
  const [paused, setPaused] = useState(false)
  const [actionError, setActionError] = useState('')
  const [actionBusy, setActionBusy] = useState(false)
  const [previewFrames, setPreviewFrames] = useState<Record<string, { capturedAt: string | null; linkGeneration: string | null }>>({})
  const videoAreaRef = useRef<HTMLDivElement | null>(null)
  const [videoAreaSize, setVideoAreaSize] = useState({ width: 0, height: 0 })
  // 首次核验转为在线可以建流；已在线过的设备断连后仍需显式重连，不恢复旧人工会话。
  useEffect(() => {
    if (device.connectionState === 'online') hasBeenOnline.current = true
    else if (hasBeenOnline.current) setSessionAllowed(false)
  }, [device.connectionState])
  const enabled = device.connectionState === 'online' && sessionAllowed
  const session = useMediaSession(device.deviceId, mode, enabled)
  const canControl = mode === 'control' && session.status === 'connected' && session.ready && !!session.sessionId
  const rotated = Math.round(displayRotation / 90) % 2 !== 0
  const videoRatio = session.width > 0 && session.height > 0
    ? rotated ? session.height / session.width : session.width / session.height
    : 1
  const frameWidth = Math.min(videoAreaSize.width, videoAreaSize.height * videoRatio)
  const frameHeight = frameWidth / videoRatio

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
    const popup = window.open('about:blank', '_blank', 'popup,width=324,height=760')
    if (!popup) { setPopupBlocked(true); return }
    popup.opener = null
    popup.location.replace(url)
    setPopupBlocked(false)
  }

  const runCommand = async (type: string, payload: Record<string, unknown> = {}) => {
    if (!canControl || actionBusy) return false
    setActionBusy(true)
    setActionError('')
    try {
      const reply = await session.command(type, payload)
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(reply.status)) throw new Error(reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`)
      return true
    } catch (error) { setActionError(errorMessage(error)); return false }
    finally { setActionBusy(false) }
  }

  const pressVolume = async (keyCode: number) => {
    if (!canControl || actionBusy) return
    setActionBusy(true)
    setActionError('')
    try {
      const down = await session.command('key', { keyCode, action: 0 })
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(down.status)) throw new Error(down.message || down.code || `设备拒绝了按键（${down.status}）`)
      const up = await session.command('key', { keyCode, action: 1 })
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(up.status)) throw new Error(up.message || up.code || `设备拒绝了按键（${up.status}）`)
    } catch (error) {
      session.release()
      setActionError(errorMessage(error))
    } finally { setActionBusy(false) }
  }

  const takeScreenshot = async () => {
    if (!session.sessionId) return
    setActionError('')
    try { await downloadScreenshot(session.sessionId) }
    catch (error) { setActionError(errorMessage(error)) }
  }

  const requestFullscreen = async () => {
    const container = videoAreaRef.current
    if (!container) return
    try { await container.requestFullscreen() }
    catch (error) { setActionError(errorMessage(error)) }
  }

  const stateLabel = device.connectionState !== 'online'
    ? deviceStatus(device)
    : session.status === 'failed' ? '视频连接失败'
      : session.status === 'closed' ? '视频已关闭'
        : session.ready ? '画面已就绪'
          : session.status === 'connected' ? '视频已连接，等待画面' : '正在连接视频'
  const badgeVariant = device.connectionState !== 'online' || session.status === 'failed' ? 'destructive' : 'outline'

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          {onBack && <Button variant="outline" size="icon" aria-label="返回设备池" onClick={onBack}><ArrowLeft /></Button>}
          <div className="min-w-0"><h1 className="truncate text-xl font-semibold tracking-tight">{deviceName(device)}</h1><p className="mt-1 text-xs text-muted-foreground">{device.links.map(link => link.kind === 'usb' ? 'USB' : 'Wi-Fi').join(' · ')} · {deviceStatus(device)}</p></div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <ToggleGroup variant="outline" value={[mode]} onValueChange={value => { if (value[0] === 'readonly' || value[0] === 'control') onModeChange(value[0]) }} aria-label="会话模式">
            <ToggleGroupItem value="readonly">只读查看</ToggleGroupItem><ToggleGroupItem value="control">控制设备</ToggleGroupItem>
          </ToggleGroup>
          <Button variant="outline" onClick={openCompact}><ExternalLink data-icon="inline-start" />独立小窗</Button>
        </div>
      </div>
      {popupBlocked && <Alert variant="destructive"><AlertTitle>浏览器阻止了小窗</AlertTitle><AlertDescription>允许此网站打开弹窗后，再点击“独立小窗”。</AlertDescription></Alert>}
      <div className="grid grid-cols-[88px_minmax(0,1fr)] items-start gap-3 md:grid-cols-[220px_minmax(0,1fr)] md:gap-5">
        <aside className="sticky top-4 min-w-0" aria-label="设备预览列表">
          <ToggleGroup orientation="vertical" variant="outline" className="max-h-[calc(100svh-160px)] w-full flex-col overflow-y-auto" value={[device.deviceId]} onValueChange={value => {
            const next = devices.find(item => item.deviceId === value[0])
            if (next) onDeviceChange(next)
          }} aria-label="切换设备">
            {devices.map(item => {
              const capturedAt = previewFrames[item.deviceId]?.capturedAt
              return (
                <ToggleGroupItem key={item.deviceId} value={item.deviceId} aria-label={`切换到${deviceName(item)}`} className="h-auto w-full flex-col gap-2 p-2 md:flex-row md:justify-start md:gap-3 md:py-3">
                  <DevicePreview device={item} compact onFrameInfoChange={info => setPreviewFrames(current => ({ ...current, [item.deviceId]: info }))} />
                  <span className="flex min-w-0 flex-col items-start gap-2 text-left">
                    <span className="w-full whitespace-normal text-xs md:text-sm">{deviceName(item)}</span>
                    <Badge variant={item.connectionState === 'offline' || item.connectionState === 'unauthorized' ? 'destructive' : 'secondary'}>{deviceStatus(item)}</Badge>
                    <span className="hidden text-xs text-muted-foreground md:inline">{capturedAt ? `拍摄于 ${new Date(capturedAt).toLocaleTimeString('zh-CN')}` : item.connectionState === 'online' ? '等待设备预览' : '暂无实时预览'}</span>
                  </span>
                </ToggleGroupItem>
              )
            })}
          </ToggleGroup>
        </aside>
        <div className="grid min-w-0 items-start gap-5 xl:grid-cols-[minmax(0,1fr)_300px]">
          <section className="flex min-w-0 flex-col gap-0 overflow-hidden rounded-xl border bg-card" aria-label="设备画面">
            <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3"><Badge variant={badgeVariant}>{stateLabel}</Badge><span className="text-xs text-muted-foreground">{session.width > 0 && session.height > 0 ? `实际 ${session.width} × ${session.height} · ${session.framesPerSecond.toFixed(1)} fps` : '实际尺寸 — · 帧率 —'}</span></div>
            <div ref={videoAreaRef} className="dark relative flex min-h-[440px] items-center justify-center bg-background px-4 py-6 text-foreground sm:min-h-[550px]">
              <div className="overflow-hidden rounded-3xl border border-border bg-card" style={{ width: frameWidth, height: frameHeight }}>
                <VideoSurface session={session} mode={mode} rotation={displayRotation} mirrored={mirrored} paused={paused} className="size-full overflow-hidden rounded-3xl" />
              </div>
              {!session.ready && <div className="pointer-events-none absolute inset-0 flex items-center justify-center px-4"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState !== 'online' ? <Unplug /> : <Monitor />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState !== 'online' ? '设备当前不可用' : session.status === 'failed' ? '视频连接失败' : '等待设备画面'}</EmptyTitle><EmptyDescription>{needsVerification ? '在设备管理中核验身份后即可投屏。' : session.error || (device.connectionState !== 'online' ? deviceStatus(device) : '正在等待设备视频流')}</EmptyDescription></EmptyHeader></Empty></div>}
            </div>
            <div className="flex flex-wrap items-center justify-center gap-2 px-3 py-3" aria-label="设备快捷操作">
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label={lastPowerCommand === false ? '唤醒屏幕' : '息屏'} title={lastPowerCommand === false ? '唤醒屏幕' : '息屏'} onClick={() => { const on = lastPowerCommand === false; void runCommand('displayPower', { on }).then(sent => { if (sent) setLastPowerCommand(on) }) }}><Power /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="音量增加" title="音量增加" onClick={() => void pressVolume(24)}><Volume2 /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="音量降低" title="音量降低" onClick={() => void pressVolume(25)}><Volume2 className="rotate-180" /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="旋转手机" title="旋转手机" onClick={() => void runCommand('rotate')}><RotateCw /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!session.ready} aria-label="旋转显示" title="旋转显示" onClick={() => setDisplayRotation(current => (current + 90) % 360)}><RotateCcw /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!session.ready} aria-label={mirrored ? '取消镜像显示' : '镜像显示'} aria-pressed={mirrored} title={mirrored ? '取消镜像显示' : '镜像显示'} onClick={() => setMirrored(current => !current)}><FlipHorizontal2 /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!session.ready} aria-label={paused ? '继续画面' : '暂停画面'} aria-pressed={paused} title={paused ? '继续画面' : '暂停画面'} onClick={() => setPaused(current => !current)}>{paused ? <Play /> : <Pause />}</Button>
              <Button size="icon-lg" variant="ghost" disabled={!session.sessionId} aria-label="截图" title="截图" onClick={() => void takeScreenshot()}><Camera /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!session.ready} aria-label="全屏" title="全屏" onClick={() => void requestFullscreen()}><Expand /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="通知面板" title="通知面板" onClick={() => void runCommand('notification')}><Unplug /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="系统设置" title="系统设置" onClick={() => void runCommand('settings')}><Monitor /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="收起通知面板" title="收起通知面板" onClick={() => void runCommand('collapse')}><ChevronDown /></Button>
              <Button size="icon-lg" variant="ghost" disabled={!canControl || actionBusy} aria-label="重置视频" title="重置视频" onClick={() => void runCommand('resetVideo')}><RefreshCw /></Button>
              <Button size="icon-lg" variant="ghost" disabled={device.connectionState !== 'online'} aria-label="重新连接视频" title="重新连接视频" onClick={() => { setSessionAllowed(false); window.setTimeout(() => setSessionAllowed(true), 0) }}><RotateCw /></Button>
            </div>
            {actionError && <Alert variant="destructive" className="mx-3 mb-3"><AlertDescription>{actionError}</AlertDescription></Alert>}
          </section>
          <div className="flex min-w-0 flex-col gap-4">
            <Card>
              <CardHeader><CardTitle>当前设备</CardTitle><CardDescription>{device.links.length} 条已登记链路</CardDescription></CardHeader>
              <CardContent className="flex flex-col gap-4">
                <div className="flex flex-wrap gap-2">{device.tags.map(tag => <Badge key={tag} variant="secondary">{tag}</Badge>)}</div>
                <Separator />
                <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-3 text-sm"><dt className="text-muted-foreground">连接状态</dt><dd className="text-right">{needsVerification ? '已连接，待核验' : connectionLabels[device.connectionState]}</dd><dt className="text-muted-foreground">分配状态</dt><dd className="text-right">{allocationLabels[device.allocationState]}</dd><dt className="text-muted-foreground">视频状态</dt><dd className="text-right">{sessionStatusLabels[session.status]}</dd></dl>
              </CardContent>
            </Card>
            <Card>
              <CardHeader><CardTitle>工作台工具</CardTitle><CardDescription>{mode === 'readonly' ? '当前为只读查看' : canControl ? '控制会话已连接' : '设备操作等待连接'}</CardDescription></CardHeader>
              <CardContent><WorkbenchTools device={device} mode={mode} session={session} /></CardContent>
            </Card>
          </div>
        </div>
      </div>
    </div>
  )
}
