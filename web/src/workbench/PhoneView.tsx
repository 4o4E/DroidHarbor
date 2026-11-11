import { useEffect, useRef, useState } from 'react'
import { Camera, ChevronDown, ChevronLeft, Circle, FlipHorizontal2, Monitor, Pause, Play, Power, RefreshCw, RotateCcw, RotateCw, Square, Unplug, Volume2 } from 'lucide-react'
import { toast } from '@/components/ui/toast'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { VideoSurface } from '@/media/VideoSurface'
import type { MediaSession } from '@/media/types'
import { deviceDisplaySize, deviceNeedsVerification, type Device, type SessionMode } from '@/lib/types'
import { errorMessage } from '@/lib/api'
import { PHONE_TOOLBAR_WIDTH } from './compactWindowSize'
import { downloadScreenshot } from '@/lib/screenshots'

type Props = { device: Device; mode: SessionMode; session: MediaSession; availableArea: { width: number; height: number }; stateLabel: string; onReconnect: () => void }

export function PhoneView({ device, mode, session, availableArea, stateLabel, onReconnect }: Props) {
  const [lastPowerCommand, setLastPowerCommand] = useState(false)
  const [displayRotation, setDisplayRotation] = useState(0)
  const [mirrored, setMirrored] = useState(false)
  const [paused, setPaused] = useState(false)
  const [actionBusy, setActionBusy] = useState(false)
  const [rotatingPhone, setRotatingPhone] = useState(false)
  const phoneRotation = useRef<{ landscape: boolean; timer?: number; observed: boolean; cancelled: boolean } | null>(null)
  const needsVerification = deviceNeedsVerification(device)
  const canControl = mode === 'control' && session.status === 'connected' && session.ready && !!session.sessionId
  const savedSize = deviceDisplaySize(device)
  const width = session.width || savedSize?.width || 0
  const height = session.height || savedSize?.height || 0
  const rotated = Math.round(displayRotation / 90) % 2 !== 0
  const ratio = width && height ? rotated ? height / width : width / height : 0
  const frameWidth = ratio ? Math.min(Math.max(0, availableArea.width - PHONE_TOOLBAR_WIDTH), availableArea.height * ratio) : 0
  const frameHeight = ratio ? frameWidth / ratio : 0

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
    setLastPowerCommand(false)
    setRotatingPhone(false)
    return () => {
      const pending = phoneRotation.current
      if (pending) { pending.cancelled = true; window.clearTimeout(pending.timer) }
      phoneRotation.current = null
    }
  }, [mode])
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
  return <div className="phone-arrangement" style={{ width: ratio ? frameWidth + PHONE_TOOLBAR_WIDTH : '100%', height: ratio ? frameHeight : '100%' }}>
    <div className="phone-toolbar" style={{ width: PHONE_TOOLBAR_WIDTH }} aria-label="设备快捷操作">
      <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="返回" title="返回" onClick={() => void runCommand('back')}><ChevronLeft /></Button>
      <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="Home" title="Home" onClick={() => void runCommand('home')}><Circle /></Button>
      <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label="最近任务" title="最近任务" onClick={() => void runCommand('recent')}><Square /></Button>
      <Button size="icon-sm" variant="ghost" disabled={!canControl || actionBusy} aria-label={lastPowerCommand ? '关闭实体屏幕' : '点亮实体屏幕'} title={lastPowerCommand ? '关闭实体屏幕' : '点亮实体屏幕'} onClick={() => { const on = !lastPowerCommand; void runCommand('displayPower', { on }).then(sent => { if (sent) setLastPowerCommand(on) }) }}><Power /></Button>
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
      <Button size="icon-sm" variant="ghost" disabled={device.connectionState !== 'online'} aria-label="重新连接视频" title="重新连接视频" onClick={onReconnect}><RotateCw /></Button>
    </div>
    <div className="phone-frame" style={{ width: ratio ? frameWidth : '100%', height: ratio ? frameHeight : '100%' }}><VideoSurface session={session} mode={mode} rotation={displayRotation} mirrored={mirrored} paused={paused} className="size-full" />{!session.hasFrame && <div className="phone-placeholder"><Empty><EmptyHeader><EmptyMedia variant="icon">{device.connectionState !== 'online' ? <Unplug /> : <Monitor />}</EmptyMedia><EmptyTitle>{needsVerification ? '设备待核验' : device.connectionState !== 'online' ? '设备当前不可用' : session.status === 'failed' ? '视频连接失败' : '等待设备画面'}</EmptyTitle><EmptyDescription>{needsVerification ? '在设备管理中核验身份后即可投屏。' : session.error || stateLabel}</EmptyDescription></EmptyHeader></Empty></div>}</div>
  </div>
}
