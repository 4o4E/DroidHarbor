import { useCallback, useEffect, useRef, useState } from 'react'
import { Camera, ChevronLeft, Circle, Monitor, RotateCw, Square, Unplug } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { VideoSurface } from '@/media/VideoSurface'
import { useMediaSession } from '@/media/useMediaSession'
import { useAuth } from '@/lib/auth'
import { useDevices, useGlobalVideoSettings, usePlatformEvents, useSystemPreferences } from '@/lib/queries'
import { deviceDisplaySize, deviceName, deviceNeedsVerification, deviceStatus, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { downloadScreenshot } from '@/lib/screenshots'
import { useErrorNotification, useVideoFallbackNotifications } from '@/lib/notifications'
import { compactWindowSize, PHONE_TOOLBAR_WIDTH } from './compactWindowSize'
import { useDeviceRuntimeNotification, useInitializedDevice } from '@/devices/DeviceMetrics'
import { WindowScaleButton } from './WindowScaleButton'
import { PreviewAudio } from '@/media/PreviewAudio'
import { useCompactTarget } from './useCompactTarget'

type CompactWorkbenchProps = { initialDeviceId: string | null; initialMode: string | null }

export function CompactWorkbench({ initialDeviceId, initialMode }: CompactWorkbenchProps) {
  const target = useCompactTarget(initialDeviceId, initialMode)
  const [scale, setScale] = useState<number | null>(null)
  const { session, loading: authLoading, error: authError } = useAuth()
  const devicesQuery = useDevices()
  const events = usePlatformEvents()
  const globalVideo = useGlobalVideoSettings()
  useErrorNotification('无法读取全局画质', globalVideo.error)
  useVideoFallbackNotifications(globalVideo.data?.fallbacks, devicesQuery.data, target.deviceId ?? undefined)
  const invalidMode = target.mode !== null && target.mode !== 'readonly' && target.mode !== 'control'
  const device = devicesQuery.data?.find(item => item.deviceId === target.deviceId)
  const mode: SessionMode = target.mode === 'control' ? 'control' : 'readonly'
  useErrorNotification('设备状态通知异常', events.error)
  useErrorNotification('无法读取设备', devicesQuery.error)

  useEffect(() => {
    document.title = device && !invalidMode ? `${deviceName(device)}${mode === 'control' ? '' : ' · 只读'}` : '页面不可用'
  }, [device, invalidMode, mode])

  if (invalidMode || !target.deviceId) return <Unavailable invalidMode={invalidMode} />
  if (authLoading || (!!session && devicesQuery.isPending)) return <main className="grid h-svh place-items-center p-4"><p className="text-sm text-muted-foreground">正在验证会话并读取设备…</p></main>
  if (!session) return <Unavailable invalidMode={false} message={authError || '此独立窗口没有有效管理员会话，请登录后从设备池重新打开。'} />
  if (devicesQuery.isError) return <Unavailable invalidMode={false} message={errorMessage(devicesQuery.error)} />
  if (!device) return <Unavailable invalidMode={false} message="没有找到指定设备，请从设备池选择设备。" />

  return <CompactSession key={`${device.deviceId}:${mode}`} device={device} mode={mode} scale={scale} onScaleChange={setScale} />
}

function Unavailable({ invalidMode, message }: { invalidMode: boolean; message?: string }) {
  return <main className="p-4"><Empty className="border"><EmptyHeader><EmptyMedia variant="icon"><Unplug /></EmptyMedia><EmptyTitle>无法打开工作台</EmptyTitle><EmptyDescription>{message || (invalidMode ? '查看模式无效，请从设备池重新打开。' : '没有找到指定设备，请从设备池选择设备。')}</EmptyDescription></EmptyHeader><EmptyContent><Button nativeButton={false} render={<a href="/" />} variant="outline">返回设备池</Button></EmptyContent></Empty></main>
}

function CompactSession({ device, mode, scale, onScaleChange }: { device: Device; mode: SessionMode; scale: number | null; onScaleChange: (value: number) => void }) {
  const [sessionAllowed, setSessionAllowed] = useState(true)
  const preferences = useSystemPreferences()
  useErrorNotification('无法读取小窗设置', preferences.error)
  useDeviceRuntimeNotification(device)
  const initialized = useInitializedDevice(device)
  const needsVerification = deviceNeedsVerification(device)
  const [busy, setBusy] = useState(false)
  const cancelResize = useRef<(() => void) | null>(null)
  const initializedSize = useRef(false)
  const screenAreaRef = useRef<HTMLDivElement | null>(null)
  const [availableArea, setAvailableArea] = useState({ width: 0, height: 0 })
  const session = useMediaSession(device.deviceId, mode, initialized && sessionAllowed)
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

  const resizeWindow = useCallback((nextScale: number, width = session.width || savedSize?.width || 0, height = session.height || savedSize?.height || 0) => {
    if (!width || !height) return
    cancelResize.current?.()
    const size = compactWindowSize(width, height, nextScale)
    onScaleChange(nextScale)
    if (size.fitted) toast.add({ title: '小窗已完整适配桌面', description: `${nextScale}×超过可用桌面，已等比缩放。`, type: 'warning' })
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
  }, [session.width, session.height, savedSize?.width, savedSize?.height, onScaleChange])
  useEffect(() => {
    if (initializedSize.current || !savedSize || !preferences.data) return
    initializedSize.current = true
    resizeWindow(scale ?? preferences.data.windowScales[0], savedSize.width, savedSize.height)
  }, [savedSize, preferences.data, resizeWindow, scale])

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
            {preferences.data && <WindowScaleButton scales={preferences.data.windowScales} value={scale ?? preferences.data.windowScales[0]} disabled={!ratio} onChange={resizeWindow} />}
            </div>
            <div className="phone-toolbar-navigation">
            <PreviewAudio session={session} mode={mode} />
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
            <Button size="icon-sm" variant="ghost" disabled={!canControl || busy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
            </div>
          </div>
          <div className="phone-frame" style={{ width: ratio ? frameWidth : '100%', height: ratio ? frameHeight : '100%' }} aria-label="设备画面">
            <VideoSurface session={session} mode={mode} className="size-full" />
            {!session.ready && <div className="phone-placeholder"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState === 'online' ? <Monitor /> : <Unplug />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState === 'online' ? session.status === 'recovering' ? '正在恢复视频' : session.status === 'failed' ? '视频连接失败' : '等待设备画面' : '设备已离线'}</EmptyTitle><EmptyDescription>{needsVerification ? '在主页面完成手机授权和刷新后即可投屏。' : deviceStatus(device)}</EmptyDescription></EmptyHeader></Empty></div>}
          </div>
        </div>
      </section>
    </main>
  )
}
