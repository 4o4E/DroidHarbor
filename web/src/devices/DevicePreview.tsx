import { useCallback, useEffect, useRef, useState, type RefObject } from 'react'
import { ImageOff, LoaderCircle, MonitorSmartphone, Unplug } from 'lucide-react'
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
}

export function DevicePreview({ device, compact = false, videoRef, videoReady = false }: DevicePreviewProps) {
  const rootRef = useRef<HTMLDivElement>(null)
  const [intersecting, setIntersecting] = useState(false)
  const [documentVisible, setDocumentVisible] = useState(() => typeof document !== 'undefined' && document.visibilityState === 'visible')
  const [lastFrame, setLastFrame] = useState<{ deviceId: string; linkSignature: string; capturedAt: string; linkGeneration: string }>()
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
  const handlePreviewFrame = useCallback((frame: DevicePreviewFrame | null) => {
    setLastFrame(frame ? {
      deviceId: device.deviceId,
      linkSignature,
      capturedAt: frame.capturedAt,
      linkGeneration: frame.linkGeneration,
    } : undefined)
  }, [device.deviceId, linkSignature])

  useEffect(() => {
    if (!online) setLastFrame(undefined)
  }, [online])
  const previousLinkIdentity = useRef(linkIdentity)
  useEffect(() => {
    if (previousLinkIdentity.current !== linkIdentity) {
      previousLinkIdentity.current = linkIdentity
      setLastFrame(undefined)
    }
  }, [linkIdentity])

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
        {online && active ? videoRef ? (
          <BrowserVideoPreview videoRef={videoRef} ready={videoReady} device={device} />
        ) : (
          <LivePreview
            key={`${device.deviceId}:${linkSignature}`}
            device={device}
            compact={compact}
            linkSignature={linkSignature}
            onPreviewFrame={handlePreviewFrame}
          />
        ) : (
          <PreviewStatus device={device} compact={compact} />
        )}
      </div>
      {!compact && <PreviewFooter device={device} active={active} capturedAt={currentFrame?.capturedAt} />}
    </div>
  )
}

// The selected device reuses its decoded video; it does not run a second ADB
// screenshot loop. Draw at most four times a second, at the preview's pixel size.
function BrowserVideoPreview({ videoRef, ready, device }: { videoRef: RefObject<HTMLVideoElement | null>; ready: boolean; device: Device }) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const [drawn, setDrawn] = useState(false)
  const [error, setError] = useState('')
  useErrorNotification('设备预览更新失败', error)
  useEffect(() => {
    setDrawn(false)
    setError('')
    const canvas = canvasRef.current
    const video = videoRef.current
    if (!ready || !canvas || !video) return
    const context = canvas.getContext('2d')
    if (!context) {
      setError('浏览器无法创建设备预览画布')
      console.error('browser_preview_context_unavailable', { deviceId: device.deviceId })
      return
    }
    let callback = 0
    let lastDraw = -Infinity
    const draw = (now: number) => {
      try {
        if (now - lastDraw >= 250 && video.videoWidth > 0 && video.videoHeight > 0 && video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
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
          }
        }
        callback = video.requestVideoFrameCallback(draw)
      } catch (cause) {
        console.error('browser_preview_capture_failed', { deviceId: device.deviceId, errorType: cause instanceof Error ? cause.name : typeof cause })
        setError(errorMessage(cause))
      }
    }
    draw(performance.now())
    return () => video.cancelVideoFrameCallback(callback)
  }, [device.deviceId, ready, videoRef])
  return <>
    <canvas ref={canvasRef} aria-label={`${device.displayName}的实时视频预览`} className={cn('max-h-full max-w-full', !drawn && 'hidden')} />
    {!drawn && <Empty className="p-1"><EmptyHeader><EmptyMedia variant="icon">{error ? <ImageOff /> : <LoaderCircle className="animate-spin" />}</EmptyMedia></EmptyHeader></Empty>}
  </>
}

function LivePreview({ device, compact, linkSignature, onPreviewFrame }: {
  device: Device
  compact: boolean
  linkSignature: string
  onPreviewFrame: (frame: DevicePreviewFrame | null) => void
}) {
  const query = useDevicePreview(device.deviceId, linkSignature)
  const initialFrame = useRef(query.data)
  const frame = query.isError || query.data === initialFrame.current ? undefined : query.data
  const [image, setImage] = useState<{ frame: DevicePreviewFrame; url: string }>()

  useEffect(() => {
    if (!frame) return
    const url = URL.createObjectURL(frame.blob)
    setImage({ frame, url })
    return () => URL.revokeObjectURL(url)
  }, [frame])
  useEffect(() => {
    if (query.isError) onPreviewFrame(null)
    else if (frame) onPreviewFrame(frame)
  }, [frame, query.isError, onPreviewFrame])

  const imageUrl = image && frame && image.frame === frame ? image.url : undefined
  if (imageUrl && frame) {
    return (
      <img
        key={`${device.deviceId}:${frame.linkGeneration}:${frame.capturedAt}`}
        src={imageUrl}
        alt={`${device.displayName}的设备画面`}
        className="h-full w-full object-contain"
      />
    )
  }

  const message = query.isError ? errorMessage(query.error) : '正在获取设备预览'
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant="icon">{query.isError ? <ImageOff /> : <LoaderCircle className="animate-spin" />}</EmptyMedia>
        {!compact && <EmptyTitle>{query.isError ? '预览暂不可用' : '正在获取预览'}</EmptyTitle>}
        {!compact && <EmptyDescription>{message}</EmptyDescription>}
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
