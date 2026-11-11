import { useCallback, useEffect, useRef, useState } from 'react'
import { Camera, ChevronLeft, Circle, Expand, Monitor, RotateCw, Square, Unplug, ZoomIn, ZoomOut } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { VideoSurface } from '@/media/VideoSurface'
import { useMediaSession } from '@/media/useMediaSession'
import { useAuth } from '@/lib/auth'
import { useDevices, usePlatformEvents } from '@/lib/queries'
import { deviceDisplaySize, deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { downloadScreenshot } from '@/lib/screenshots'
import { useErrorNotification } from '@/lib/notifications'
import { compactWindowSize, PHONE_TOOLBAR_WIDTH } from './compactWindowSize'

type CompactWorkbenchProps = { initialDeviceId: string | null; initialMode: string | null }

export function CompactWorkbench({ initialDeviceId, initialMode }: CompactWorkbenchProps) {
  const { session, loading: authLoading, error: authError } = useAuth()
  const devicesQuery = useDevices()
  const events = usePlatformEvents()
  const invalidMode = initialMode !== null && initialMode !== 'readonly' && initialMode !== 'control'
  const device = devicesQuery.data?.find(item => item.deviceId === initialDeviceId)
  const mode: SessionMode = initialMode === 'control' ? 'control' : 'readonly'
  useErrorNotification('设备状态通知异常', events.error)
  useErrorNotification('无法读取设备', devicesQuery.error)

  useEffect(() => {
    document.title = device && !invalidMode ? `${deviceName(device)}${mode === 'control' ? '' : ' · 只读'}` : '页面不可用'
  }, [device, invalidMode, mode])

  if (invalidMode || !initialDeviceId) return <Unavailable invalidMode={invalidMode} />
  if (authLoading || (!!session && devicesQuery.isPending)) return <main className="grid h-svh place-items-center p-4"><p className="text-sm text-muted-foreground">正在验证会话并读取设备…</p></main>
  if (!session) return <Unavailable invalidMode={false} message={authError || '此独立窗口没有有效管理员会话，请登录后从设备池重新打开。'} />
  if (devicesQuery.isError) return <Unavailable invalidMode={false} message={errorMessage(devicesQuery.error)} />
  if (!device) return <Unavailable invalidMode={false} message="没有找到指定设备，请从设备池选择设备。" />

  return <CompactSession key={device.deviceId} device={device} mode={mode} />
}

function Unavailable({ invalidMode, message }: { invalidMode: boolean; message?: string }) {
  return <main className="p-4"><Empty className="border"><EmptyHeader><EmptyMedia variant="icon"><Unplug /></EmptyMedia><EmptyTitle>无法打开工作台</EmptyTitle><EmptyDescription>{message || (invalidMode ? '查看模式无效，请从设备池重新打开。' : '没有找到指定设备，请从设备池选择设备。')}</EmptyDescription></EmptyHeader><EmptyContent><Button nativeButton={false} render={<a href="/" />} variant="outline">返回设备池</Button></EmptyContent></Empty></main>
}

function CompactSession({ device, mode }: { device: Device; mode: SessionMode }) {
  const [sessionAllowed, setSessionAllowed] = useState(true)
  const hasBeenOnline = useRef(device.connectionState === 'online')
  const needsVerification = deviceNeedsVerification(device)
  const [busy, setBusy] = useState(false)
  const cancelResize = useRef<(() => void) | null>(null)
  const initializedSize = useRef(false)
  const screenAreaRef = useRef<HTMLDivElement | null>(null)
  const [availableArea, setAvailableArea] = useState({ width: 0, height: 0 })
  useEffect(() => {
    if (device.connectionState === 'online') hasBeenOnline.current = true
    else if (hasBeenOnline.current) setSessionAllowed(false)
  }, [device.connectionState])
  const session = useMediaSession(device.deviceId, mode, device.connectionState === 'online' && sessionAllowed)
  const canControl = mode === 'control' && session.status === 'connected' && session.ready && !!session.sessionId
  const savedSize = deviceDisplaySize(device)
  const savedRatio = savedSize ? savedSize.width / savedSize.height : 0
  const ratio = session.width > 0 && session.height > 0 ? session.width / session.height : savedRatio
  const frameWidth = ratio ? Math.min(Math.max(0, availableArea.width - PHONE_TOOLBAR_WIDTH), availableArea.height * ratio) : 0
  const frameHeight = ratio ? frameWidth / ratio : 0

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

  const resizeWindow = useCallback((fraction: number, targetRatio = ratio) => {
    if (!targetRatio) return
    cancelResize.current?.()
    const size = compactWindowSize(targetRatio, fraction)
    if (size.innerHeight < 120) {
      toast.add({ title: '无法调整小窗尺寸', description: '当前屏幕可用空间不足。', type: 'error', timeout: 0 })
      return
    }
    const finish = () => {
      window.clearTimeout(timer)
      window.removeEventListener('resize', observeResize)
      cancelResize.current = null
    }
    const matches = () => Math.abs(window.innerWidth - size.innerWidth) <= 2 && Math.abs(window.innerHeight - size.innerHeight) <= 2
    const observeResize = () => { if (matches()) finish() }
    const timer = window.setTimeout(() => {
      finish()
      if (!matches()) toast.add({ title: '无法调整小窗尺寸', description: '浏览器未允许调整到目标尺寸，可手动调整窗口。', type: 'error', timeout: 0 })
    }, 1000)
    cancelResize.current = finish
    window.addEventListener('resize', observeResize)
    window.resizeTo(size.innerWidth + size.chromeWidth, size.innerHeight + size.chromeHeight)
    observeResize()
  }, [ratio])
  useEffect(() => {
    if (initializedSize.current || !savedRatio) return
    initializedSize.current = true
    resizeWindow(0.8, savedRatio)
  }, [savedRatio, resizeWindow])

  const runCommand = async (type: string) => {
    if (!canControl || busy) return
    setBusy(true)
    try {
      const reply = await session.command(type, {})
      if (!['succeeded', 'waiting-key-frame', 'dispatched'].includes(reply.status)) throw new Error(reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`)
    }
    catch (error) { toast.add({ title: '设备操作未完成', description: errorMessage(error), type: 'error', timeout: 0 }) }
    finally { setBusy(false) }
  }

  const screenshot = async () => {
    if (!session.sessionId) return
    try { await downloadScreenshot(session.sessionId) }
    catch (error) { toast.add({ title: '截图失败', description: errorMessage(error), type: 'error', timeout: 0 }) }
  }

  return (
    <main className="compact-workbench">
      <section ref={screenAreaRef} className="phone-stage" aria-label={`${deviceName(device)}的小窗`}>
        <div className="phone-arrangement" style={{ width: ratio ? frameWidth + PHONE_TOOLBAR_WIDTH : '100%', height: ratio ? frameHeight : '100%' }}>
          <div className="phone-toolbar" style={{ width: PHONE_TOOLBAR_WIDTH }} aria-label="小窗操作">
            <div className="phone-toolbar-actions">
            <Button size="icon-sm" variant="ghost" disabled={!session.sessionId} aria-label="截图" title="截图" onClick={() => void screenshot()}><Camera /></Button>
            <Button size="icon-sm" variant="ghost" disabled={device.connectionState !== 'online'} aria-label="重新连接视频" title="重新连接视频" onClick={() => { setSessionAllowed(false); window.setTimeout(() => setSessionAllowed(true), 0) }}><RotateCw /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!ratio} aria-label="小窗口" title="小窗口" onClick={() => resizeWindow(0.6)}><ZoomOut /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!ratio} aria-label="标准窗口" title="标准窗口" onClick={() => resizeWindow(0.8)}><Expand /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!ratio} aria-label="大窗口" title="大窗口" onClick={() => resizeWindow(1)}><ZoomIn /></Button>
            </div>
            <div className="phone-toolbar-navigation">
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
            </div>
          </div>
          <div className="phone-frame" style={{ width: ratio ? frameWidth : '100%', height: ratio ? frameHeight : '100%' }} aria-label="设备画面">
            <VideoSurface session={session} mode={mode} className="size-full" />
            {!session.ready && <div className="phone-placeholder"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState === 'online' ? <Monitor /> : <Unplug />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState === 'online' ? session.status === 'failed' ? '视频连接失败' : '等待设备画面' : '设备已离线'}</EmptyTitle><EmptyDescription>{needsVerification ? '在主页面核验设备身份后即可投屏。' : session.error || deviceStatus(device)}</EmptyDescription></EmptyHeader></Empty></div>}
          </div>
        </div>
      </section>
    </main>
  )
}
