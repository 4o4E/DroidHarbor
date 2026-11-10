import { useEffect, useRef, useState } from 'react'
import { Camera, ChevronLeft, Circle, Expand, Monitor, RotateCw, Square, Unplug, X, ZoomIn, ZoomOut } from 'lucide-react'
import { Alert, AlertAction, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { VideoSurface } from '@/media/VideoSurface'
import { useMediaSession } from '@/media/useMediaSession'
import { useAuth } from '@/lib/auth'
import { useDevices, usePlatformEvents } from '@/lib/queries'
import { deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { downloadScreenshot } from '@/lib/screenshots'

type CompactWorkbenchProps = { initialDeviceId: string | null; initialMode: string | null }

export function CompactWorkbench({ initialDeviceId, initialMode }: CompactWorkbenchProps) {
  const { session, loading: authLoading, error: authError } = useAuth()
  const devicesQuery = useDevices()
  const events = usePlatformEvents()
  const [resizeError, setResizeError] = useState('')
  const invalidMode = initialMode !== null && initialMode !== 'readonly' && initialMode !== 'control'
  const device = devicesQuery.data?.find(item => item.deviceId === initialDeviceId)
  const mode: SessionMode = initialMode === 'control' ? 'control' : 'readonly'

  useEffect(() => {
    document.title = device && !invalidMode ? `${deviceName(device)}${mode === 'control' ? '' : ' · 只读'}` : '页面不可用'
  }, [device, invalidMode, mode])

  if (invalidMode || !initialDeviceId) return <Unavailable invalidMode={invalidMode} />
  if (authLoading || (!!session && devicesQuery.isPending)) return <main className="grid h-svh place-items-center p-4"><p className="text-sm text-muted-foreground">正在验证会话并读取设备…</p></main>
  if (!session) return <Unavailable invalidMode={false} message={authError || '此独立窗口没有有效管理员会话，请登录后从设备池重新打开。'} />
  if (devicesQuery.isError) return <Unavailable invalidMode={false} message={errorMessage(devicesQuery.error)} />
  if (!device) return <Unavailable invalidMode={false} message="没有找到指定设备，请从设备池选择设备。" />

  return <CompactSession key={device.deviceId} device={device} mode={mode} resizeError={resizeError} setResizeError={setResizeError} eventStatus={events.status} />
}

function Unavailable({ invalidMode, message }: { invalidMode: boolean; message?: string }) {
  return <main className="p-4"><Empty className="border"><EmptyHeader><EmptyMedia variant="icon"><Unplug /></EmptyMedia><EmptyTitle>无法打开工作台</EmptyTitle><EmptyDescription>{message || (invalidMode ? '查看模式无效，请从设备池重新打开。' : '没有找到指定设备，请从设备池选择设备。')}</EmptyDescription></EmptyHeader><EmptyContent><Button nativeButton={false} render={<a href="/" />} variant="outline">返回设备池</Button></EmptyContent></Empty></main>
}

function CompactSession({ device, mode, resizeError, setResizeError, eventStatus }: { device: Device; mode: SessionMode; resizeError: string; setResizeError: (value: string) => void; eventStatus: string }) {
  const [sessionAllowed, setSessionAllowed] = useState(true)
  const hasBeenOnline = useRef(device.connectionState === 'online')
  const needsVerification = deviceNeedsVerification(device)
  const [actionError, setActionError] = useState('')
  const [busy, setBusy] = useState(false)
  const cancelResize = useRef<(() => void) | null>(null)
  const screenAreaRef = useRef<HTMLDivElement | null>(null)
  const [availableArea, setAvailableArea] = useState({ width: 0, height: 0 })
  useEffect(() => {
    if (device.connectionState === 'online') hasBeenOnline.current = true
    else if (hasBeenOnline.current) setSessionAllowed(false)
  }, [device.connectionState])
  const session = useMediaSession(device.deviceId, mode, device.connectionState === 'online' && sessionAllowed)
  const canControl = mode === 'control' && session.status === 'connected' && session.ready && !!session.sessionId
  const ratio = session.width > 0 && session.height > 0 ? session.width / session.height : 1
  const frameWidth = Math.min(availableArea.width, availableArea.height * ratio)
  const frameHeight = frameWidth / ratio

  useEffect(() => {
    const area = screenAreaRef.current
    if (!area) return
    const observer = new ResizeObserver(entries => {
      const { width, height } = entries[0].contentRect
      setAvailableArea(current => current.width === width && current.height === height ? current : { width, height })
    })
    observer.observe(area)
    return () => observer.disconnect()
  }, [])

  useEffect(() => () => cancelResize.current?.(), [])

  const resizeWindow = (fraction: number) => {
    cancelResize.current?.()
    setResizeError('')
    const chromeWidth = Math.max(0, window.outerWidth - window.innerWidth)
    const chromeHeight = Math.max(0, window.outerHeight - window.innerHeight)
    const maximumPictureHeight = Math.min(
      window.screen.availHeight - chromeHeight - 64,
      (window.screen.availWidth - chromeWidth - 24) / ratio,
    )
    const pictureHeight = Math.floor(maximumPictureHeight * fraction)
    const width = Math.round(pictureHeight * ratio)
    const height = pictureHeight + 40
    if (pictureHeight < 120 || width + chromeWidth > window.screen.availWidth) {
      setResizeError('当前屏幕可用空间不足，无法调整到该尺寸。')
      return
    }
    const finish = () => {
      window.clearTimeout(timer)
      window.removeEventListener('resize', observeResize)
      cancelResize.current = null
    }
    const observeResize = () => {
      if (Math.abs(window.innerWidth - width) <= 2 && Math.abs(window.innerHeight - height) <= 2) finish()
    }
    const timer = window.setTimeout(() => {
      finish()
      if (Math.abs(window.innerWidth - width) > 2 || Math.abs(window.innerHeight - height) > 2) setResizeError('浏览器未允许调整到目标尺寸。可手动调整窗口，或从工作台重新打开小窗。')
    }, 1000)
    cancelResize.current = finish
    window.addEventListener('resize', observeResize)
    window.resizeTo(width + chromeWidth, height + chromeHeight)
    observeResize()
  }

  const runCommand = async (type: string) => {
    if (!canControl || busy) return
    setBusy(true)
    setActionError('')
    try {
      const reply = await session.command(type, {})
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(reply.status)) throw new Error(reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`)
    }
    catch (error) { setActionError(errorMessage(error)) }
    finally { setBusy(false) }
  }

  const screenshot = async () => {
    if (!session.sessionId) return
    setActionError('')
    try { await downloadScreenshot(session.sessionId) }
    catch (error) { setActionError(errorMessage(error)) }
  }

  return (
    <main className="dark h-svh bg-background text-foreground">
      <section className="flex h-full flex-col" aria-label={`${deviceName(device)}的小窗`}>
        <div ref={screenAreaRef} className="flex min-h-0 flex-1 items-center justify-center">
          <div className="relative flex items-center justify-center" style={{ width: frameWidth, height: frameHeight }} aria-label="设备画面">
            <VideoSurface session={session} mode={mode} className="size-full overflow-hidden rounded-3xl border border-border bg-card" />
            {!session.ready && <div className="pointer-events-none absolute inset-0 flex items-center justify-center"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState === 'online' ? <Monitor /> : <Unplug />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState === 'online' ? session.status === 'failed' ? '视频连接失败' : '等待设备画面' : '设备已离线'}</EmptyTitle><EmptyDescription>{needsVerification ? '在主页面核验设备身份后即可投屏。' : session.error || deviceStatus(device)}</EmptyDescription></EmptyHeader></Empty></div>}
          </div>
        </div>
        {resizeError && <Alert variant="destructive"><AlertDescription>{resizeError}</AlertDescription><AlertAction><Button size="icon-xs" variant="ghost" aria-label="关闭提示" onClick={() => setResizeError('')}><X /></Button></AlertAction></Alert>}
        {(actionError || session.error) && <Alert variant="destructive"><AlertDescription>{actionError || session.error}</AlertDescription></Alert>}
        {eventStatus === 'disconnected' && <Alert variant="destructive"><AlertDescription>设备状态事件暂时断开，当前画面状态可能不是最新。</AlertDescription></Alert>}
        <div className="flex h-10 shrink-0 items-center overflow-x-auto border-t px-2" aria-label="小窗底部操作">
          <div className="mx-auto flex shrink-0 items-center gap-1">
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!session.sessionId} aria-label="截图" title="截图" onClick={() => void screenshot()}><Camera /></Button>
            <Button size="icon-sm" variant="ghost" disabled={device.connectionState !== 'online'} aria-label="重新连接视频" title="重新连接视频" onClick={() => { setSessionAllowed(false); window.setTimeout(() => setSessionAllowed(true), 0) }}><RotateCw /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!session.width || !session.height} aria-label="小窗口" title="小窗口" onClick={() => resizeWindow(0.6)}><ZoomOut /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!session.width || !session.height} aria-label="标准窗口" title="标准窗口" onClick={() => resizeWindow(0.8)}><Expand /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!session.width || !session.height} aria-label="大窗口" title="大窗口" onClick={() => resizeWindow(1)}><ZoomIn /></Button>
          </div>
        </div>
      </section>
    </main>
  )
}
