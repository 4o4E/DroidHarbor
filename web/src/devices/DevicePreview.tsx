import { useCallback, useEffect, useLayoutEffect, useRef, useState, type RefObject } from 'react'
import { ImageOff, MonitorSmartphone, Unplug } from 'lucide-react'
import { cn } from 'cn'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { errorMessage } from '@/lib/api'
import { deviceDisplaySize, type Device } from '@/lib/types'
import { useErrorNotification } from '@/lib/notifications'
import { useDevicePreview, type DevicePreviewFrame } from './useDevicePreview'

export type DevicePreviewProps = {
  device: Device
  compact?: boolean
  videoRef?: RefObject<HTMLVideoElement | null>
  videoReady?: boolean
  poll?: boolean
  captureRef?: RefObject<(() => void) | null>
}

export function DevicePreview({ device, compact = false, videoRef, videoReady = false, poll = true, captureRef }: DevicePreviewProps) {
  const rootRef = useRef<HTMLDivElement>(null)
  const [intersecting, setIntersecting] = useState(false)
  const [documentVisible, setDocumentVisible] = useState(() => typeof document !== 'undefined' && document.visibilityState === 'visible')
  const [lastFrame, setLastFrame] = useState<{ deviceId: string; linkSignature: string; capturedAt: string; linkGeneration: string }>()
  const [videoFrameIdentity, setVideoFrameIdentity] = useState('')
  const online = device.connectionState === 'online'
  const displaySize = deviceDisplaySize(device)
  const linkSignature = JSON.stringify(device.links
    .map(({ linkId, kind, endpoint, connectionState, identityVerified }) => [linkId, kind, endpoint, connectionState, identityVerified] as const)
    .sort((left, right) => {
      const leftKey = JSON.stringify(left)
      const rightKey = JSON.stringify(right)
      return leftKey < rightKey ? -1 : leftKey > rightKey ? 1 : 0
    }))

  useEffect(() => {
    const element = rootRef.current
    if (!element) return
    const observer = new IntersectionObserver(([entry]) => setIntersecting(entry.isIntersecting), { threshold: 0.01 })
    observer.observe(element)
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    const updateVisibility = () => setDocumentVisible(document.visibilityState === 'visible')
    updateVisibility()
    document.addEventListener('visibilitychange', updateVisibility)
    return () => document.removeEventListener('visibilitychange', updateVisibility)
  }, [])

  const active = online && intersecting && documentVisible
  const linkIdentity = JSON.stringify([device.deviceId, linkSignature])
  const currentFrame = online && lastFrame?.deviceId === device.deviceId && lastFrame.linkSignature === linkSignature ? lastFrame : undefined
  const hasVideoFrame = online && videoFrameIdentity === linkIdentity
  const handleVideoFrame = useCallback(() => setVideoFrameIdentity(linkIdentity), [linkIdentity])
  const handlePreviewFrame = useCallback((frame: DevicePreviewFrame | null) => {
    setLastFrame(frame ? {
      deviceId: device.deviceId,
      linkSignature,
      capturedAt: frame.capturedAt,
      linkGeneration: frame.linkGeneration,
    } : undefined)
  }, [device.deviceId, linkSignature])

  useEffect(() => {
    if (!online) { setLastFrame(undefined); setVideoFrameIdentity('') }
  }, [online])
  const previousLinkIdentity = useRef(linkIdentity)
  if (previousLinkIdentity.current !== linkIdentity) {
    previousLinkIdentity.current = linkIdentity
    setLastFrame(undefined)
    setVideoFrameIdentity('')
  }

  return (
    <div
      ref={rootRef}
      className={cn('overflow-hidden border', compact && 'device-list-preview')}
      aria-label={`${device.displayName}的画面预览`}
      title={compact ? device.displayName : currentFrame
        ? `${device.displayName} · 拍摄于 ${new Date(currentFrame.capturedAt).toLocaleString('zh-CN')}`
        : `${device.displayName} · ${online ? '暂无最新预览' : '设备不可用'}`}
    >
      <div className={cn('flex w-full items-center justify-center bg-background text-foreground', !compact && 'h-56')} style={compact && displaySize ? { aspectRatio: `${displaySize.width} / ${displaySize.height}` } : undefined}>
        <BrowserVideoPreview key={linkIdentity} videoRef={videoRef} ready={videoReady} active={online && active} visible={online} device={device} onFrame={handleVideoFrame} captureRef={captureRef} />
        {!hasVideoFrame && (online ? (
          <LivePreview
            key={linkIdentity}
            device={device}
            compact={compact}
            linkSignature={linkSignature}
            enabled={active && !videoReady && device.runtime?.powerState === 'active' && (poll || device.runtime?.coverState === 'ready')}
            poll={poll}
            onPreviewFrame={handlePreviewFrame}
          />
        ) : (
          <PreviewStatus device={device} compact={compact} />
        ))}
      </div>
      {!compact && <PreviewFooter device={device} active={active} capturedAt={currentFrame?.capturedAt} />}
    </div>
  )
}

// Keep the canvas mounted when selection changes. The explicit capture callback
// samples the final presented frame before foreground ownership changes.
function BrowserVideoPreview({ videoRef, ready, active, visible, device, onFrame, captureRef }: {
  videoRef?: RefObject<HTMLVideoElement | null>
  ready: boolean
  active: boolean
  visible: boolean
  device: Device
  onFrame: () => void
  captureRef?: RefObject<(() => void) | null>
}) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const [drawn, setDrawn] = useState(false)
  const [error, setError] = useState('')
  useErrorNotification('设备预览更新失败', error)
  useLayoutEffect(() => {
    setError('')
    const canvas = canvasRef.current
    const video = videoRef?.current
    if (!canvas || !video) return
    const context = canvas.getContext('2d')
    if (!context) {
      setError('浏览器无法创建设备预览画布')
      console.error('browser_preview_context_unavailable', { deviceId: device.deviceId })
      return
    }
    let callback = 0
    let lastDraw = -Infinity
    const capture = (now: number, force: boolean) => {
      try {
        if ((force || now - lastDraw >= 250) && video.videoWidth > 0 && video.videoHeight > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
          const width = canvas.parentElement?.clientWidth ?? 0
          if (width > 0) {
            const scale = Math.min(1, width * window.devicePixelRatio / video.videoWidth, 1280 / Math.max(video.videoWidth, video.videoHeight))
            const targetWidth = Math.max(1, Math.round(video.videoWidth * scale))
            const targetHeight = Math.max(1, Math.round(video.videoHeight * scale))
            if (canvas.width !== targetWidth || canvas.height !== targetHeight) {
              canvas.width = targetWidth
              canvas.height = targetHeight
            }
            context.drawImage(video, 0, 0, targetWidth, targetHeight)
            lastDraw = now
            setDrawn(true)
            onFrame()
          }
        }
        return true
      } catch (cause) {
        console.error('browser_preview_capture_failed', { deviceId: device.deviceId, errorType: cause instanceof Error ? cause.name : typeof cause })
        setError(errorMessage(cause))
        return false
      }
    }
    const captureLatest = () => capture(performance.now(), true)
    if (captureRef) captureRef.current = captureLatest
    const draw = (now: number) => {
      if (capture(now, false)) callback = video.requestVideoFrameCallback(draw)
    }
    if (ready && active) draw(performance.now())
    return () => {
      captureLatest()
      video.cancelVideoFrameCallback(callback)
      if (captureRef?.current === captureLatest) captureRef.current = null
    }
  }, [active, captureRef, device.deviceId, onFrame, ready, videoRef])
  return <>
    <canvas ref={canvasRef} aria-label={`${device.displayName}的实时视频预览`} className={cn('max-h-full max-w-full', (!drawn || !visible) && 'hidden')} />
  </>
}

function LivePreview({ device, compact, linkSignature, enabled, poll, onPreviewFrame }: {
  device: Device
  compact: boolean
  linkSignature: string
  enabled: boolean
  poll: boolean
  onPreviewFrame: (frame: DevicePreviewFrame | null) => void
}) {
  const query = useDevicePreview(device.deviceId, linkSignature, enabled, poll)
  const frame = query.data
  const [image, setImage] = useState<{ frame: DevicePreviewFrame; url: string }>()
  const [decodeError, setDecodeError] = useState('')
  useErrorNotification('设备预览更新失败', decodeError || query.error)

  useEffect(() => {
    if (!frame) return
    const url = URL.createObjectURL(frame.blob)
    let active = true
    const decoded = new Image()
    decoded.src = url
    void decoded.decode().then(() => {
      if (active) {
        setImage({ frame, url })
        setDecodeError('')
      }
    }).catch(cause => {
      if (active) setDecodeError(errorMessage(cause))
    })
    return () => { active = false; URL.revokeObjectURL(url) }
  }, [frame])
  useEffect(() => {
    if (image) onPreviewFrame(image.frame)
  }, [image, onPreviewFrame])

  const imageUrl = image?.url
  if (imageUrl) {
    return (
      <img
        src={imageUrl}
        alt={`${device.displayName}的设备画面`}
        className="h-full w-full object-contain"
      />
    )
  }

  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">{query.isError || decodeError ? <ImageOff /> : <MonitorSmartphone />}</EmptyMedia>
        {!compact && <EmptyTitle>暂无预览</EmptyTitle>}
      </EmptyHeader>
    </Empty>
  )
}

function PreviewStatus({ device, compact }: { device: Device; compact: boolean }) {
  const offline = device.connectionState === 'offline'
  const unauthorized = device.connectionState === 'unauthorized'
  const title = offline ? '设备离线' : unauthorized ? '设备未授权' : '预览已暂停'
  const description = offline
    ? '连接恢复后更新画面'
    : unauthorized
      ? '检查设备连接权限后重试'
      : '滚动进入视野且页面可见时更新'

  return (
    <Empty className={cn(compact && 'p-1')}>
      <EmptyHeader>
        <EmptyMedia variant="icon">{offline ? <Unplug /> : <MonitorSmartphone />}</EmptyMedia>
        {!compact && <><EmptyTitle>{title}</EmptyTitle><EmptyDescription>{description}</EmptyDescription></>}
      </EmptyHeader>
    </Empty>
  )
}

function PreviewFooter({ device, active, capturedAt }: { device: Device; active: boolean; capturedAt?: string }) {
  const updatedAt = capturedAt ? new Date(capturedAt).toLocaleTimeString('zh-CN') : '—'
  const label = device.connectionState !== 'online'
    ? '暂停更新'
    : active
      ? '每 5 秒更新'
      : '进入视野后更新'

  return (
    <div className="flex items-center justify-between gap-2 px-3 py-2 text-xs text-muted-foreground">
      <span>拍摄于 {updatedAt}</span>
      <span>{label}</span>
    </div>
  )
}
