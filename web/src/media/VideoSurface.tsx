import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ClipboardEvent as ReactClipboardEvent, type CompositionEvent, type KeyboardEvent, type MouseEvent, type PointerEvent, type WheelEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import type { ControlReply, MediaSession, SessionMode } from './types'

type VideoSurfaceProps = {
  session: MediaSession
  mode: SessionMode
  rotation?: number
  mirrored?: boolean
  paused?: boolean
  className?: string
}

type PointerPosition = { localId: number; x: number; y: number; virtualId?: number; invertX?: boolean; invertY?: boolean }

const MAX_SIMULTANEOUS_POINTERS = 10

const WHEEL_DELTA_MODE_LINE = 1
const WHEEL_DELTA_MODE_PAGE = 2
const WHEEL_LINES_PER_STEP = 3
const WHEEL_PIXELS_PER_STEP = 100

// Browser pixel deltas use 100 CSS px per SDK wheel step; 3 line units represent one step.
function normalizeWheelDelta(delta: number, mode: number, pagePixels: number): number {
  if (!Number.isFinite(delta)) return 0
  if (mode === WHEEL_DELTA_MODE_LINE) return delta / WHEEL_LINES_PER_STEP
  if (mode === WHEEL_DELTA_MODE_PAGE) return delta * pagePixels / WHEEL_PIXELS_PER_STEP
  return delta / WHEEL_PIXELS_PER_STEP
}

function virtualPosition(point: { x: number; y: number }, invertX: boolean, invertY: boolean, width: number, height: number) {
  return {
    x: invertX ? width - 1 - point.x : point.x,
    y: invertY ? height - 1 - point.y : point.y,
  }
}

const keyCodes: Record<string, number> = {
  ArrowDown: 20,
  ArrowLeft: 21,
  ArrowRight: 22,
  ArrowUp: 19,
  AltLeft: 57,
  AltRight: 58,
  Backspace: 67,
  ControlLeft: 113,
  ControlRight: 114,
  Delete: 112,
  End: 123,
  Enter: 66,
  Escape: 111,
  Home: 122,
  Insert: 124,
  MetaLeft: 117,
  MetaRight: 118,
  PageDown: 93,
  PageUp: 92,
  ShiftLeft: 59,
  ShiftRight: 60,
  Space: 62,
  Tab: 61,
}

function androidKeyCode(code: string): number | undefined {
  if (code in keyCodes) return keyCodes[code]
  if (/^Key[A-Z]$/.test(code)) return 29 + code.charCodeAt(3) - 65
  if (/^Digit[0-9]$/.test(code)) return 7 + Number(code[5])
  if (/^Numpad[0-9]$/.test(code)) return 144 + Number(code[6])
  if (/^F([1-9]|1[0-2])$/.test(code)) return 131 + Number(code.slice(1)) - 1
  return undefined
}

function metaState(event: KeyboardEvent): number {
  return (event.shiftKey ? 1 : 0) | (event.altKey ? 2 : 0) | (event.ctrlKey ? 0x1000 : 0) | (event.metaKey ? 0x10000 : 0)
}

function describeReply(reply: ControlReply): string {
  return reply.message || reply.code || `设备拒绝了 ${reply.type} 操作（${reply.status}）`
}

export function VideoSurface({ session, mode, rotation = 0, mirrored = false, paused = false, className }: VideoSurfaceProps) {
  const [controlError, setControlError] = useState('')
  const pointersRef = useRef(new Map<number, PointerPosition>())
  const mouseButtonsRef = useRef(new Map<number, string>())
  const keysRef = useRef(new Map<string, number>())
  const suppressedKeysRef = useRef(new Set<string>())
  const nextPointerRef = useRef(0)
  const pendingMovesRef = useRef(new Map<number, { pointerId: number; x: number; y: number }>())
  const moveFrameRef = useRef<number | null>(null)
  const surfaceRef = useRef<HTMLDivElement | null>(null)
  const videoFrameRef = useRef<HTMLDivElement | null>(null)
  const [stageSize, setStageSize] = useState({ width: 0, height: 0 })
  const releaseInputsRef = useRef<() => void>(() => {})
  const sessionIdentityRef = useRef({ sessionId: session.sessionId, epoch: session.epoch })
  const mountedRef = useRef(true)
  useLayoutEffect(() => {
    if (sessionIdentityRef.current.sessionId !== session.sessionId || sessionIdentityRef.current.epoch !== session.epoch) {
      pointersRef.current.clear()
      mouseButtonsRef.current.clear()
      keysRef.current.clear()
      suppressedKeysRef.current.clear()
      pendingMovesRef.current.clear()
      if (moveFrameRef.current !== null) {
        window.cancelAnimationFrame(moveFrameRef.current)
        moveFrameRef.current = null
      }
      setControlError('')
    }
    sessionIdentityRef.current = { sessionId: session.sessionId, epoch: session.epoch }
  }, [session.epoch, session.sessionId])
  useEffect(() => {
    mountedRef.current = true
    return () => { mountedRef.current = false }
  }, [])
  const canControl = mode === 'control' && session.status === 'connected' && session.ready
  const quarterTurn = ((Math.round(rotation / 90) % 4) + 4) % 4
  const rotated = quarterTurn % 2 === 1
  const displayWidth = rotated ? session.height : session.width
  const displayHeight = rotated ? session.width : session.height
  useEffect(() => {
    const surface = surfaceRef.current
    if (!surface) return
    const resizeStage = () => {
      if (displayWidth <= 0 || displayHeight <= 0) {
        setStageSize(current => current.width === 0 && current.height === 0 ? current : { width: 0, height: 0 })
        return
      }
      const scale = Math.min(surface.clientWidth / displayWidth, surface.clientHeight / displayHeight)
      const width = Math.max(0, displayWidth * scale)
      const height = Math.max(0, displayHeight * scale)
      setStageSize(current => current.width === width && current.height === height ? current : { width, height })
    }
    resizeStage()
    const observer = new ResizeObserver(resizeStage)
    observer.observe(surface)
    return () => observer.disconnect()
  }, [displayHeight, displayWidth])

  const send = useCallback((type: string, payload?: Record<string, unknown>) => {
    const identity = { sessionId: session.sessionId, epoch: session.epoch }
    if (sessionIdentityRef.current.sessionId !== identity.sessionId || sessionIdentityRef.current.epoch !== identity.epoch) return
    void session.command(type, payload).then(reply => {
      if (!mountedRef.current || sessionIdentityRef.current.sessionId !== identity.sessionId || sessionIdentityRef.current.epoch !== identity.epoch) return
      if (!['succeeded', 'dispatched', 'waiting-key-frame', 'received', 'acknowledged'].includes(reply.status)) setControlError(describeReply(reply))
    }).catch(cause => {
      if (mountedRef.current && sessionIdentityRef.current.sessionId === identity.sessionId && sessionIdentityRef.current.epoch === identity.epoch) {
        setControlError(cause instanceof Error ? cause.message : '设备控制失败')
      }
    })
  }, [session.command, session.epoch, session.sessionId])

  // Convert from the actually rendered/rotated rectangle back to source pixels.
  const coordinates = useCallback((clientX: number, clientY: number, clamp = false) => {
    const frame = videoFrameRef.current
    if (!frame || session.width <= 0 || session.height <= 0) return null
    const rect = frame.getBoundingClientRect()
    if (rect.width <= 0 || rect.height <= 0) return null
    const outside = clientX < rect.left || clientX > rect.right || clientY < rect.top || clientY > rect.bottom
    if (outside && !clamp) return null
    let u = (clientX - rect.left) / rect.width
    let v = (clientY - rect.top) / rect.height
    if (clamp) {
      u = Math.max(0, Math.min(1, u))
      v = Math.max(0, Math.min(1, v))
    }
    if (mirrored) u = 1 - u
    let x = u
    let y = v
    if (quarterTurn === 1) [x, y] = [v, 1 - u]
    else if (quarterTurn === 2) [x, y] = [1 - u, 1 - v]
    else if (quarterTurn === 3) [x, y] = [1 - v, u]
    return {
      x: Math.max(0, Math.min(session.width - 1, Math.floor(x * session.width))),
      y: Math.max(0, Math.min(session.height - 1, Math.floor(y * session.height))),
    }
  }, [mirrored, quarterTurn, session.height, session.width])

  const releaseLocalInputs = useCallback(() => {
    pointersRef.current.clear()
    mouseButtonsRef.current.clear()
    keysRef.current.clear()
    suppressedKeysRef.current.clear()
    pendingMovesRef.current.clear()
    if (moveFrameRef.current !== null) {
      window.cancelAnimationFrame(moveFrameRef.current)
      moveFrameRef.current = null
    }
    session.release()
  }, [session.release])
  useLayoutEffect(() => {
    releaseInputsRef.current = releaseLocalInputs
  }, [releaseLocalInputs])


  useEffect(() => {
    const onWindowBlur = () => releaseInputsRef.current()
    const onVisibility = () => {
      if (document.visibilityState !== 'visible') releaseInputsRef.current()
    }
    window.addEventListener('blur', onWindowBlur)
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      window.removeEventListener('blur', onWindowBlur)
      document.removeEventListener('visibilitychange', onVisibility)
      if (moveFrameRef.current !== null) window.cancelAnimationFrame(moveFrameRef.current)
      releaseInputsRef.current()
    }
  }, [])

  const wasControlRef = useRef(canControl)
  useEffect(() => {
    if (wasControlRef.current && !canControl) releaseLocalInputs()
    wasControlRef.current = canControl
  }, [canControl, releaseLocalInputs])

  useEffect(() => {
    const video = session.videoRef.current
    if (!video) return
    if (paused) video.pause()
    else if (session.status === 'connected') {
      const identity = sessionIdentityRef.current
      void video.play().catch(cause => {
        if (mountedRef.current && sessionIdentityRef.current.sessionId === identity.sessionId && sessionIdentityRef.current.epoch === identity.epoch) {
          setControlError(cause instanceof Error ? cause.message : '视频播放失败')
        }
      })
    }
  }, [paused, session.epoch, session.sessionId, session.status, session.videoRef])

  const onPointerDown = (event: PointerEvent<HTMLDivElement>) => {
    if (!canControl) return
    const point = coordinates(event.clientX, event.clientY)
    if (!point) return
    if (event.button !== 0) {
      const button = mouseButtonName(event.button)
      if (!button) return
      event.preventDefault()
      event.currentTarget.focus({ preventScroll: true })
      try {
        event.currentTarget.setPointerCapture(event.pointerId)
      } catch (cause) {
        console.warn('pointer_capture_failed', { errorType: cause instanceof Error ? cause.name : typeof cause })
        setControlError('无法保持鼠标按键捕获，请重新点击。')
        return
      }
      mouseButtonsRef.current.set(event.pointerId, button)
      send('mouseButton', { button, action: 0 })
      return
    }
    event.preventDefault()

    // The SDK mouse locks its Ctrl/Shift virtual finger until this pointer goes up.
    const useVirtualFinger = event.pointerType === 'mouse' && (event.ctrlKey || event.shiftKey)
    const activeIds = new Set<number>()
    for (const pointer of pointersRef.current.values()) {
      activeIds.add(pointer.localId)
      if (pointer.virtualId !== undefined) activeIds.add(pointer.virtualId)
    }
    const needed = useVirtualFinger ? 2 : 1
    const allocated: number[] = []
    let candidate = nextPointerRef.current
    for (let attempt = 0; attempt < MAX_SIMULTANEOUS_POINTERS && allocated.length < needed; attempt++) {
      if (!activeIds.has(candidate)) allocated.push(candidate)
      candidate = (candidate + 1) % MAX_SIMULTANEOUS_POINTERS
    }
    if (allocated.length < needed) {
      setControlError('同时触控数量已达到设备协议上限')
      return
    }
    event.currentTarget.focus({ preventScroll: true })
    try { event.currentTarget.setPointerCapture(event.pointerId) } catch (cause) {
      console.warn('pointer_capture_failed', { errorType: cause instanceof Error ? cause.name : typeof cause })
    }
    nextPointerRef.current = candidate
    const localId = allocated[0]
    const virtualId = useVirtualFinger ? allocated[1] : undefined
    const pointer: PointerPosition = { localId, ...point }
    if (virtualId !== undefined) {
      pointer.virtualId = virtualId
      // Ctrl inverts both axes, Shift only X, and Ctrl+Shift only Y.
      pointer.invertX = event.ctrlKey !== event.shiftKey
      pointer.invertY = event.ctrlKey
    }
    pointersRef.current.set(event.pointerId, pointer)
    send('touch', { pointerId: localId, action: 0, ...point, width: session.width, height: session.height, pressure: event.pressure || 1 })
    if (virtualId !== undefined) {
      const virtual = virtualPosition(pointer, pointer.invertX === true, pointer.invertY === true, session.width, session.height)
      send('touch', { pointerId: virtualId, action: 0, ...virtual, width: session.width, height: session.height, pressure: 1 })
    }
  }

  const onPointerMove = (event: PointerEvent<HTMLDivElement>) => {
    if (!canControl) return
    const active = pointersRef.current.get(event.pointerId)
    if (!active) return
    const point = coordinates(event.clientX, event.clientY, true)
    if (!point) return
    event.preventDefault()
    active.x = point.x
    active.y = point.y
    pendingMovesRef.current.set(active.localId, { pointerId: active.localId, ...point })
    if (active.virtualId !== undefined) {
      const virtual = virtualPosition(active, active.invertX === true, active.invertY === true, session.width, session.height)
      pendingMovesRef.current.set(active.virtualId, { pointerId: active.virtualId, ...virtual })
    }
    if (moveFrameRef.current === null) {
      moveFrameRef.current = window.requestAnimationFrame(() => {
        moveFrameRef.current = null
        const moves = [...pendingMovesRef.current.values()]
        pendingMovesRef.current.clear()
        for (const move of moves) send('touch', { ...move, action: 2, width: session.width, height: session.height, pressure: 1 })
      })
    }
  }

  const onPointerEnd = (event: PointerEvent<HTMLDivElement>, cancelled = false) => {
    const button = mouseButtonsRef.current.get(event.pointerId)
    if (button) {
      mouseButtonsRef.current.delete(event.pointerId)
      event.preventDefault()
      if (canControl) send('mouseButton', { button, action: 1 })
      if (!cancelled && event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
      return
    }
    const active = pointersRef.current.get(event.pointerId)
    if (!active) return
    pointersRef.current.delete(event.pointerId)
    pendingMovesRef.current.delete(active.localId)
    if (active.virtualId !== undefined) pendingMovesRef.current.delete(active.virtualId)
    if (canControl) {
      event.preventDefault()
      const point = coordinates(event.clientX, event.clientY, true) ?? active
      send('touch', { pointerId: active.localId, action: 1, x: point.x, y: point.y, width: session.width, height: session.height, pressure: 0 })
      if (active.virtualId !== undefined) {
        const virtual = virtualPosition(point, active.invertX === true, active.invertY === true, session.width, session.height)
        send('touch', { pointerId: active.virtualId, action: 1, ...virtual, width: session.width, height: session.height, pressure: 0 })
      }
    }
    if (!cancelled && event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId)
  }

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!canControl || event.nativeEvent.isComposing) return
    if ((event.ctrlKey || event.metaKey) && event.code === 'KeyV') {
      suppressedKeysRef.current.add(event.code)
      return
    }
    const keyCode = androidKeyCode(event.code)
    if (keyCode === undefined) return
    event.preventDefault()
    keysRef.current.set(event.code, keyCode)
    send('key', { keyCode, action: 0, repeat: event.repeat ? 1 : 0, metaState: metaState(event) })
  }

  const onKeyUp = (event: KeyboardEvent<HTMLDivElement>) => {
    if (suppressedKeysRef.current.delete(event.code)) {
      event.preventDefault()
      return
    }
    const keyCode = keysRef.current.get(event.code)
    if (keyCode === undefined) return
    keysRef.current.delete(event.code)
    if (!canControl) return
    event.preventDefault()
    send('key', { keyCode, action: 1, repeat: 0, metaState: metaState(event) })
  }

  const onPaste = (event: ReactClipboardEvent<HTMLDivElement>) => {
    if (!canControl) return
    event.preventDefault()
    event.stopPropagation()
    const text = event.clipboardData.getData('text/plain')
    if (text) send('clipboard.paste', { text })
  }

  const onCompositionEnd = (event: CompositionEvent<HTMLDivElement>) => {
    if (canControl && event.data) send('text', { text: event.data })
  }

  const onWheel = (event: WheelEvent<HTMLDivElement>) => {
    if (!canControl) return
    event.preventDefault()
    const point = coordinates(event.clientX, event.clientY)
    if (!point) return
    // Android SDK VSCROLL is positive up; WheelEvent.deltaY is positive down.
    const hScroll = normalizeWheelDelta(event.deltaX, event.deltaMode, Math.max(1, event.currentTarget.clientWidth))
    const vScroll = -normalizeWheelDelta(event.deltaY, event.deltaMode, Math.max(1, event.currentTarget.clientHeight))
    if (hScroll === 0 && vScroll === 0) return
    send('scroll', { ...point, width: session.width, height: session.height, hScroll, vScroll, buttons: 0 })
  }

  const mouseButtonName = (button: number): string | null => {
    if (button === 1) return 'middle'
    if (button === 2) return 'right'
    if (button === 3) return 'back'
    if (button === 4) return 'forward'
    return null
  }


  const onContextMenu = (event: MouseEvent<HTMLDivElement>) => event.preventDefault()

  return (
    <div className={`flex h-full min-h-0 min-w-0 flex-col ${className ?? ''}`}>
      {session.error && <Alert variant="destructive" className="mb-2 shrink-0"><AlertDescription>{session.error}</AlertDescription></Alert>}
      {controlError && <Alert variant="destructive" className="mb-2 shrink-0"><AlertDescription>{controlError}</AlertDescription></Alert>}
      <div
        ref={surfaceRef}
        className="relative flex min-h-0 min-w-0 flex-1 touch-none items-center justify-center overflow-hidden bg-black outline-none"
        tabIndex={canControl ? 0 : -1}
        aria-label={mode === 'control' ? '设备视频与交互区域' : '设备视频'}
        onBlur={releaseLocalInputs}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={event => onPointerEnd(event)}
        onPointerCancel={event => onPointerEnd(event, true)}
        onKeyDown={onKeyDown}
        onKeyUp={onKeyUp}
        onPaste={onPaste}
        onCompositionEnd={onCompositionEnd}
        onWheel={onWheel}
        onContextMenu={onContextMenu}
      >
        <div
          ref={videoFrameRef}
          className="relative shrink-0 overflow-hidden bg-black"
          style={{ width: stageSize.width, height: stageSize.height }}
        >
          <video
            ref={session.videoRef}
            className="absolute bg-black object-contain"
            style={{
              width: rotated ? stageSize.height : stageSize.width,
              height: rotated ? stageSize.width : stageSize.height,
              maxWidth: 'none',
              maxHeight: 'none',
              transform: `translate(-50%, -50%) scaleX(${mirrored ? -1 : 1}) rotate(${quarterTurn * 90}deg)`,
              left: '50%',
              top: '50%',
            }}
            autoPlay
            muted
            playsInline
            aria-label="实时设备画面"
          />
        </div>
        {paused && <div className="pointer-events-none absolute inset-0 flex items-center justify-center bg-black/40 text-sm text-white">画面已暂停</div>}
        {session.status === 'connecting' && <div className="pointer-events-none absolute bottom-3 rounded-md bg-black/70 px-3 py-1 text-xs text-white">正在连接设备画面…</div>}
        {session.status === 'failed' && !session.error && <div className="pointer-events-none absolute bottom-3 rounded-md bg-black/70 px-3 py-1 text-xs text-white">媒体连接失败</div>}
      </div>
    </div>
  )
}
