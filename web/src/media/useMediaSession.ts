import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, errorMessage } from '@/lib/api'
import type { SessionMode } from '@/lib/types'
import type { ControlReply, MediaSession } from './types'

const CONTROL_LABEL = 'droidharbor.control.v1'
const CONTROL_MESSAGE_LIMIT = 2 << 20
const CONTROL_BUFFER_LIMIT = 4 << 20
const MAX_PENDING_COMMANDS = 64
const MAX_DATA_CHANNEL_CHUNK = 16 << 10
const INCOMPLETE_FRAME_TIMEOUT_MS = 5_000
const COMMAND_TIMEOUT_MS = 30_000

type SessionSnapshot = {
  sessionId: string
  deviceId: string
  mode: SessionMode
  answer: string
  controlProtocol: string
}

type FrameBoundary = { epoch: number; timestamp: number; width: number; height: number }
type PendingCommand = {
  waitForTerminal: boolean
  epochBound: boolean
  resolve: (reply: ControlReply) => void
  reject: (error: Error) => void
  timeout: number
}
type WireReply = {
  sessionId?: unknown
  captureEpoch?: unknown
  sequence?: unknown
  type?: unknown
  status?: unknown
  errorCode?: unknown
  message?: unknown
  payload?: unknown
}
type RemoteStreamState = { runtime: MediaRuntime; stream: MediaStream }
type RTPVideoFrameMetadata = VideoFrameCallbackMetadata & { rtpTimestamp?: number }
type InboundControlFrame = {
  header: Uint8Array
  headerLength: number
  payload: Uint8Array | null
  payloadLength: number
  payloadRead: number
  timeout: number | null
}
type MediaRuntime = {
  deviceId: string
  mode: SessionMode
  foreground: boolean
  present: boolean
  presenceWanted: boolean
  presencePromise: Promise<void>
  decoded: boolean
  presentedBoundary: string
  active: boolean
  peer: RTCPeerConnection | null
  channel: RTCDataChannel | null
  stream: MediaStream | null
  sessionId: string | null
  epoch: number
  sequence: number
  boundary: FrameBoundary | null
  acknowledgedBoundary: string
  acknowledgingBoundary: string
  pending: Map<number, PendingCommand>
  inbound: InboundControlFrame
  teardownPromise: Promise<void> | null
  close: (status: 'closed' | 'failed', failure?: string) => Promise<void>
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function validUInt32(value: unknown): value is number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= 0xffff_ffff
}

function rtpIsAtOrAfter(timestamp: number, boundary: number): boolean {
  return ((timestamp - boundary) >>> 0) < 0x8000_0000
}

export function useMediaSession(deviceId: string, mode: SessionMode, enabled: boolean, onClipboard?: (text: string, source: 'phone' | 'browser') => void, presence?: { active: boolean; after?: Promise<void> }): MediaSession {
  const foreground = presence?.active ?? true
  const activationBarrier = presence?.after
  const clipboardCallback = useRef(onClipboard)
  useEffect(() => { clipboardCallback.current = onClipboard }, [onClipboard])
  const videoRef = useRef<HTMLVideoElement | null>(null)
  const runtimeRef = useRef<MediaRuntime | null>(null)
  const stateScope = useRef({ deviceId, mode })
  const [currentRuntime, setCurrentRuntime] = useState<MediaRuntime | null>(null)
  const [sessionId, setSessionId] = useState<string | null>(null)
  const [status, setStatus] = useState<MediaSession['status']>(enabled ? 'connecting' : 'closed')
  const [sessionError, setSessionError] = useState('')
  const [cleanupError, setCleanupError] = useState('')
  const [ready, setReady] = useState(false)
  const [hasFrame, setHasFrame] = useState(false)
  // Decoder lifetime survives capture-epoch changes; a new boundary revokes
  // presentation/input readiness without making the retained transport cold.
  const [hasDecodedVideo, setHasDecodedVideo] = useState(false)
  const [present, setPresent] = useState(true)
  const [epoch, setEpoch] = useState(0)
  const [dimensions, setDimensions] = useState({ width: 0, height: 0 })
  const [framesPerSecond, setFramesPerSecond] = useState(0)
  const [remoteState, setRemoteState] = useState<RemoteStreamState | null>(null)

  const command = useCallback((type: string, payload?: Record<string, unknown>): Promise<ControlReply> => {
    const text = typeof payload?.text === 'string' ? payload.text : ''
    const waitForTerminal = type === 'clipboard.get' || type === 'clipboard.set' || type === 'clipboard.paste' ||
      (type === 'text' && (text.length > 300 || /[^\x00-\x7f]/.test(text)))
    const runtime = currentRuntime
    const channel = runtime?.channel
    if (!runtime?.active || runtime.deviceId !== deviceId || runtime.mode !== mode || !runtime.sessionId || !channel || channel.readyState !== 'open') {
      return Promise.reject(new Error('WebRTC 控制通道尚未就绪'))
    }
    const lifecycle = type === 'suspend' || type === 'resume' || type === 'frame-ready' || type === 'ping'
    if (!lifecycle && (!runtime.foreground || !runtime.present)) return Promise.reject(new Error('后台窗口仅保留视频，请先切回该设备'))
    const sequence = ++runtime.sequence
    const message = {
      sessionId: runtime.sessionId,
      captureEpoch: runtime.epoch,
      sequence,
      type,
      payload: payload ?? {},
    }
    let encoded: Uint8Array<ArrayBuffer>
    try {
      encoded = new TextEncoder().encode(JSON.stringify(message))
    } catch (cause) {
      return Promise.reject(new Error(errorMessage(cause)))
    }
    if (encoded.byteLength === 0 || encoded.byteLength > CONTROL_MESSAGE_LIMIT) {
      return Promise.reject(new Error('控制消息超过允许大小'))
    }
    const framedByteLength = encoded.byteLength + 4
    if (runtime.pending.size >= MAX_PENDING_COMMANDS || channel.bufferedAmount + framedByteLength > CONTROL_BUFFER_LIMIT) {
      const failure = new Error('WebRTC 控制通道已达到有界发送上限，会话已关闭')
      void runtime.close('failed', failure.message).catch(cause => {
        setCleanupError(`控制通道过载后的会话清理失败：${errorMessage(cause)}`)
      })
      return Promise.reject(failure)
    }
    return new Promise<ControlReply>((resolve, reject) => {
      const timeout = window.setTimeout(() => {
        runtime.pending.delete(sequence)
        const failure = new Error('控制命令响应超时，会话已关闭')
        reject(failure)
        void runtime.close('failed', failure.message).catch(cause => {
          setCleanupError(`控制命令超时后的会话清理失败：${errorMessage(cause)}`)
        })
      }, COMMAND_TIMEOUT_MS)
      runtime.pending.set(sequence, {
        waitForTerminal,
        epochBound: type !== 'suspend' && type !== 'resume' && type !== 'ping',
        resolve: reply => {
          window.clearTimeout(timeout)
          if (type === 'clipboard.paste' && reply.status === 'acknowledged' && typeof payload?.text === 'string') clipboardCallback.current?.(payload.text, 'browser')
          if (type === 'clipboard.get' && reply.status === 'received' && typeof reply.payload?.text === 'string') clipboardCallback.current?.(reply.payload.text, 'phone')
          resolve(reply)
        },
        reject: cause => {
          window.clearTimeout(timeout)
          reject(cause)
        },
        timeout,
      })
      try {
        const header = new Uint8Array(4)
        new DataView(header.buffer).setUint32(0, encoded.byteLength, false)
        channel.send(header)
        for (let offset = 0; offset < encoded.byteLength; offset += MAX_DATA_CHANNEL_CHUNK) {
          channel.send(encoded.subarray(offset, Math.min(offset + MAX_DATA_CHANNEL_CHUNK, encoded.byteLength)))
        }
      } catch (cause) {
        const failure = `WebRTC 控制消息发送失败：${errorMessage(cause)}`
        void runtime.close('failed', failure).catch(cleanupCause => {
          setCleanupError(`控制发送失败后的会话清理失败：${errorMessage(cleanupCause)}`)
        })
      }
    })
  }, [currentRuntime, deviceId, mode])

  const changePresence = useCallback((active: boolean, nextSessionId = '', after?: Promise<void>) => {
    const runtime = currentRuntime
    if (!runtime?.active) return Promise.resolve()
    runtime.foreground = active
    if (!active) setReady(false)
    if (runtime.presenceWanted === active) return runtime.presencePromise
    runtime.presenceWanted = active
    runtime.presencePromise = runtime.presencePromise.then(async () => {
      await after
      if (!runtime.active) return
      if (!runtime.sessionId || runtime.channel?.readyState !== 'open') {
        await runtime.close('closed')
        return
      }
      const reply = await command(active ? 'resume' : 'suspend', active ? {} : { nextSessionId })
      if (!runtime.active) return
      if (reply.status !== 'succeeded') throw new Error(reply.message || '设备窗口切换未完成')
      runtime.present = active
      runtime.acknowledgedBoundary = ''
      runtime.acknowledgingBoundary = ''
      setReady(false)
      setPresent(active)
    }).catch(async cause => {
      if (runtime.active) await runtime.close('failed', errorMessage(cause))
      throw cause
    })
    return runtime.presencePromise
  }, [command, currentRuntime])
  const suspend = useCallback((nextSessionId: string) => changePresence(false, nextSessionId), [changePresence])
  useEffect(() => {
    void changePresence(foreground, '', activationBarrier).catch(cause => {
      setSessionError(`切换媒体窗口失败：${errorMessage(cause)}`)
    })
  }, [activationBarrier, changePresence, foreground])

  useEffect(() => {
    stateScope.current = { deviceId, mode }
    if (!enabled || !deviceId || (mode !== 'readonly' && mode !== 'control')) {
      setHasFrame(false)
      setHasDecodedVideo(false)
      setCurrentRuntime(null)
      setStatus('closed')
      setReady(false)
      setSessionId(null)
      setRemoteState(null)
      return
    }

    const abortController = new AbortController()
    const runtime: MediaRuntime = {
      deviceId,
      mode,
      foreground,
      present: true,
      presenceWanted: true,
      presencePromise: Promise.resolve(),
      decoded: false,
      presentedBoundary: '',
      active: true,
      peer: null,
      channel: null,
      stream: null,
      sessionId: null,
      epoch: 0,
      sequence: 0,
      boundary: null,
      acknowledgedBoundary: '',
      acknowledgingBoundary: '',
      pending: new Map(),
      inbound: { header: new Uint8Array(4), headerLength: 0, payload: null, payloadLength: 0, payloadRead: 0, timeout: null },
      teardownPromise: null,
      close: (nextStatus, failure) => teardown(nextStatus, failure),
    }
    runtimeRef.current = runtime
    setCurrentRuntime(runtime)
    const isCurrent = () => runtimeRef.current === runtime && runtime.active
    setStatus('connecting')
    setSessionError('')
    setReady(false)
    setHasFrame(false)
    setHasDecodedVideo(false)
    setPresent(true)
    setSessionId(null)
    setEpoch(0)
    setDimensions({ width: 0, height: 0 })
    setFramesPerSecond(0)
    setRemoteState(null)

    const pendingCandidates: RTCIceCandidateInit[] = []
    const sendCandidate = (candidate: RTCIceCandidateInit) => {
      if (!runtime.sessionId || !isCurrent()) return
      void api<void>(`/sessions/${encodeURIComponent(runtime.sessionId)}/candidates`, {
        method: 'POST',
        body: candidate,
        signal: abortController.signal,
      }).catch(cause => {
        if (isCurrent()) void teardown('failed', `ICE 候选提交失败：${errorMessage(cause)}`)
      })
    }

    const failPending = (cause: Error, epochOnly = false) => {
      for (const [sequence, pending] of runtime.pending) {
        if (epochOnly && !pending.epochBound) continue
        window.clearTimeout(pending.timeout)
        pending.reject(cause)
        runtime.pending.delete(sequence)
      }
    }

    const teardown = (nextStatus: 'closed' | 'failed', failure = ''): Promise<void> => {
      if (runtime.teardownPromise) return runtime.teardownPromise
      const wasCurrent = runtimeRef.current === runtime
      runtime.active = false
      abortController.abort()
      if (runtime.inbound.timeout !== null) window.clearTimeout(runtime.inbound.timeout)
      runtime.inbound.timeout = null
      runtime.inbound.payload = null
      runtime.inbound.payloadLength = 0
      runtime.inbound.payloadRead = 0
      if (wasCurrent) {
        setCurrentRuntime(null)
        setStatus(nextStatus)
        setReady(false)
        setHasFrame(false)
        setHasDecodedVideo(false)
        setPresent(false)
        setSessionId(null)
        setRemoteState(null)
        if (failure) setSessionError(failure)
      }
      failPending(new Error(failure || '媒体会话已关闭'))
      const currentSession = runtime.sessionId
      const channel = runtime.channel
      runtime.channel = null
      const peer = runtime.peer
      runtime.peer = null
      if (channel) {
        channel.onopen = null
        channel.onclose = null
        channel.onerror = null
        channel.onmessage = null
      }
      if (peer) {
        peer.ontrack = null
        peer.onconnectionstatechange = null
        peer.onicecandidate = null
      }
      pendingCandidates.length = 0
      const disposeTransport = () => {
        if (channel) {
          try { channel.close() } catch (cause) {
            console.warn('media_channel_close_failed', { sessionId: currentSession, errorType: cause instanceof Error ? cause.name : typeof cause })
          }
        }
        if (peer) {
          try { peer.close() } catch (cause) {
            console.warn('media_peer_close_failed', { sessionId: currentSession, errorType: cause instanceof Error ? cause.name : typeof cause })
          }
        }
      }
      // Normal closure ends server authority before the transport callback can
      // report an expected shutdown as a media failure.
      if (nextStatus === 'failed') disposeTransport()
      runtime.stream?.getTracks().forEach(track => track.stop())
      runtime.stream = null
      runtime.sessionId = null
      if (runtimeRef.current === runtime) runtimeRef.current = null
      const closeRequest = currentSession
        ? api<void>(`/sessions/${encodeURIComponent(currentSession)}`, { method: 'DELETE', keepalive: true })
        : Promise.resolve()
      const cleanup = closeRequest.catch(cause => {
        console.error('media_session_cleanup_failed', { sessionId: currentSession, errorType: cause instanceof Error ? cause.name : typeof cause })
        const message = `结束媒体会话失败：${errorMessage(cause)}`
        if (wasCurrent) setCleanupError(message)
        if (nextStatus === 'closed' && !failure) throw cause
        if (failure && runtimeRef.current === runtime) setSessionError(`${failure}；${message}`)
      })
      runtime.teardownPromise = cleanup.finally(disposeTransport)
      return runtime.teardownPromise
    }

    const updateConnection = () => {
      if (!isCurrent() || !runtime.peer || !runtime.channel) return
      if (runtime.peer.connectionState === 'failed' || runtime.peer.connectionState === 'disconnected') {
        void teardown(runtime.foreground ? 'failed' : 'closed', runtime.foreground ? 'WebRTC 连接已断开' : '')
        return
      }
      if (runtime.peer.connectionState === 'connected' && runtime.channel.readyState === 'open') setStatus('connected')
    }

    const processWireMessage = (data: Uint8Array) => {
      if (!isCurrent()) return
      let value: unknown
      try {
        value = JSON.parse(new TextDecoder().decode(data))
      } catch {
        void teardown('failed', '收到无效的媒体控制消息')
        return
      }
      if (!isRecord(value)) {
        void teardown('failed', '收到无效的媒体控制消息')
        return
      }
      const reply = value as WireReply
      if (reply.sessionId !== runtime.sessionId || typeof reply.type !== 'string' || typeof reply.status !== 'string') {
        void teardown('failed', '媒体控制响应不属于当前会话')
        return
      }
      if (reply.type === 'frame-boundary') {
        if (!Number.isSafeInteger(reply.captureEpoch) || Number(reply.captureEpoch) <= 0 || !isRecord(reply.payload)) {
          void teardown('failed', '收到无效的视频帧边界')
          return
        }
        const payload = reply.payload
        if (
          !validUInt32(payload.timestamp) || !Number.isInteger(payload.width) || !Number.isInteger(payload.height) ||
          Number(payload.width) <= 0 || Number(payload.height) <= 0 || Number(payload.width) > 0xffff || Number(payload.height) > 0xffff
        ) {
          void teardown('failed', '收到无效的视频帧边界')
          return
        }
        const boundary: FrameBoundary = {
          epoch: Number(reply.captureEpoch),
          timestamp: payload.timestamp,
          width: Number(payload.width),
          height: Number(payload.height),
        }
        if (runtime.epoch !== boundary.epoch) {
          failPending(new Error('画面代次已切换，请核对此前设备操作的结果。'), true)
          setHasFrame(false)
        }
        runtime.boundary = boundary
        runtime.epoch = boundary.epoch
        runtime.acknowledgedBoundary = ''
        runtime.acknowledgingBoundary = ''
        setEpoch(boundary.epoch)
        setDimensions({ width: boundary.width, height: boundary.height })
        setReady(false)
        return
      }
      if (reply.type === 'clipboard.changed') {
        if (!runtime.foreground || !runtime.present || mode !== 'control' || reply.captureEpoch !== runtime.epoch) return
        if (reply.status !== 'received' || !isRecord(reply.payload) || typeof reply.payload.text !== 'string') {
          void teardown('failed', '收到无效的剪贴板消息')
          return
        }
        clipboardCallback.current?.(reply.payload.text, 'phone')
        return
      }
      if (typeof reply.sequence !== 'number' || !Number.isSafeInteger(reply.sequence)) return
      const pending = runtime.pending.get(reply.sequence)
      if (!pending) return
      const terminalClipboardReply = reply.type === 'clipboard.result' &&
        ['received', 'acknowledged', 'timed-out', 'unknown', 'failed'].includes(reply.status)
      const terminalFailure = ['timed-out', 'unknown', 'failed'].includes(reply.status)
      if (pending.waitForTerminal && !terminalClipboardReply && !terminalFailure) return
      runtime.pending.delete(reply.sequence)
      pending.resolve({
        type: reply.type,
        status: reply.status,
        ...(typeof reply.errorCode === 'string' ? { code: reply.errorCode } : {}),
        ...(typeof reply.message === 'string' ? { message: reply.message } : {}),
        ...(isRecord(reply.payload) ? { payload: reply.payload } : {}),
      })
    }
    const consumeControlChunk = (data: unknown) => {
      if (!isCurrent()) return
      if (!(data instanceof ArrayBuffer) || data.byteLength === 0 || data.byteLength > MAX_DATA_CHANNEL_CHUNK) {
        void teardown('failed', 'WebRTC 控制通道分片格式无效')
        return
      }
      const chunk = new Uint8Array(data)
      let offset = 0
      while (offset < chunk.length && isCurrent()) {
        const inbound = runtime.inbound
        if (inbound.timeout === null) {
          inbound.timeout = window.setTimeout(() => {
            if (isCurrent()) void teardown('failed', 'WebRTC 控制消息分片接收超时')
          }, INCOMPLETE_FRAME_TIMEOUT_MS)
        }
        if (inbound.payload === null) {
          const count = Math.min(4 - inbound.headerLength, chunk.length - offset)
          inbound.header.set(chunk.subarray(offset, offset + count), inbound.headerLength)
          inbound.headerLength += count
          offset += count
          if (inbound.headerLength < 4) continue
          const length = new DataView(inbound.header.buffer).getUint32(0, false)
          if (length === 0 || length > CONTROL_MESSAGE_LIMIT) {
            void teardown('failed', 'WebRTC 控制消息长度无效')
            return
          }
          inbound.payload = new Uint8Array(length)
          inbound.payloadLength = length
          inbound.payloadRead = 0
        }
        const payload = inbound.payload
        if (!payload) return
        const count = Math.min(inbound.payloadLength - inbound.payloadRead, chunk.length - offset)
        payload.set(chunk.subarray(offset, offset + count), inbound.payloadRead)
        inbound.payloadRead += count
        offset += count
        if (inbound.payloadRead === inbound.payloadLength) {
          if (inbound.timeout !== null) window.clearTimeout(inbound.timeout)
          inbound.timeout = null
          inbound.headerLength = 0
          inbound.payload = null
          inbound.payloadLength = 0
          inbound.payloadRead = 0
          processWireMessage(payload)
        }
      }
    }

    const start = async () => {
      try {
        const peer = new RTCPeerConnection({ iceServers: [], bundlePolicy: 'max-bundle', rtcpMuxPolicy: 'require' })
        runtime.peer = peer
        const capabilities = RTCRtpReceiver.getCapabilities('video')
        const h264 = capabilities?.codecs.filter(codec => codec.mimeType.toLowerCase() === 'video/h264') ?? []
        if (h264.length === 0) throw new Error('当前浏览器没有可用的 H.264 解码能力')
        const transceiver = peer.addTransceiver('video', { direction: 'recvonly' })
        transceiver.setCodecPreferences(h264)
        const channel = peer.createDataChannel(CONTROL_LABEL, { ordered: true })
        channel.binaryType = 'arraybuffer'
        runtime.channel = channel
        channel.onopen = updateConnection
        channel.onclose = () => {
          if (isCurrent()) void teardown(runtime.foreground ? 'failed' : 'closed', runtime.foreground ? 'WebRTC 控制通道已关闭' : '')
        }
        channel.onerror = () => {
          if (isCurrent()) void teardown(runtime.foreground ? 'failed' : 'closed', runtime.foreground ? 'WebRTC 控制通道发生错误' : '')
        }
        channel.onmessage = event => consumeControlChunk(event.data)
        peer.onconnectionstatechange = updateConnection
        // Start signaling immediately; interface discovery continues in parallel.
        // Candidates collected before the session response stay with this runtime.
        peer.onicecandidate = event => {
          if (!event.candidate || !isCurrent()) return
          const candidate = event.candidate.toJSON()
          if (runtime.sessionId) sendCandidate(candidate)
          else pendingCandidates.push(candidate)
        }
        peer.ontrack = event => {
          if (!isCurrent()) return
          if (event.track.kind !== 'video') {
            void teardown('failed', '服务端发送了不支持的媒体轨道')
            return
          }
          const stream = event.streams[0] ?? new MediaStream([event.track])
          runtime.stream = stream
          setRemoteState({ runtime, stream })
          event.track.onended = () => {
            if (isCurrent()) void teardown(runtime.foreground ? 'failed' : 'closed', runtime.foreground ? '设备视频轨道已结束' : '')
          }
        }

        const offer = await peer.createOffer()
        if (!isCurrent()) return
        await peer.setLocalDescription(offer)
        if (!isCurrent()) return
        const sdp = peer.localDescription?.sdp
        if (!sdp) throw new Error('浏览器未生成 WebRTC offer')
        await activationBarrier
        if (!isCurrent()) return
        const snapshot = await api<SessionSnapshot>(`/devices/${encodeURIComponent(deviceId)}/sessions`, {
          method: 'POST',
          body: { mode, offer: sdp },
          signal: abortController.signal,
        })
        runtime.sessionId = snapshot.sessionId
        if (!isCurrent()) {
          void api<void>(`/sessions/${encodeURIComponent(snapshot.sessionId)}`, { method: 'DELETE', keepalive: true }).catch(cause => {
            setCleanupError(`迟到的媒体会话未能清理：${errorMessage(cause)}`)
          })
          return
        }
        if (!snapshot.sessionId || !snapshot.answer || snapshot.controlProtocol !== CONTROL_LABEL) {
          throw new Error('服务端返回了不兼容的媒体协商协议')
        }
        setSessionId(snapshot.sessionId)
        await peer.setRemoteDescription({ type: 'answer', sdp: snapshot.answer })
        for (const candidate of pendingCandidates) sendCandidate(candidate)
        pendingCandidates.length = 0
        if (!isCurrent()) return
      } catch (cause) {
        if (isCurrent()) {
          const message = cause instanceof DOMException && cause.name === 'AbortError' ? '媒体会话协商已取消' : errorMessage(cause)
          void teardown('failed', message)
        }
      }
    }

    const closeOnPageHide = () => {
      void teardown('closed').catch(cause => {
        console.error('media_pagehide_cleanup_failed', { deviceId, errorType: cause instanceof Error ? cause.name : typeof cause })
      })
    }
    window.addEventListener('pagehide', closeOnPageHide)
    void start()
    return () => {
      window.removeEventListener('pagehide', closeOnPageHide)
      void teardown('closed').catch(cause => {
        setCleanupError(`媒体会话卸载清理失败：${errorMessage(cause)}`)
      })
    }
  }, [deviceId, enabled, mode])

  useEffect(() => {
    const video = videoRef.current
    if (!video) return
    const stream = remoteState?.stream ?? null
    video.srcObject = stream
    if (stream) {
      void video.play().catch(cause => {
        if (remoteState && runtimeRef.current === remoteState.runtime) setSessionError(errorMessage(cause))
      })
    }
    return () => {
      if (video.srcObject === stream) video.srcObject = null
    }
  }, [remoteState])

  useEffect(() => {
    if (!remoteState) return
    const { runtime } = remoteState
    const video = videoRef.current
    if (!video || typeof video.requestVideoFrameCallback !== 'function') {
      if (runtimeRef.current === runtime) {
        setSessionError('浏览器不支持可信的视频帧时间戳，无法确认画面就绪')
        void runtime.close('failed', '浏览器不支持可信的视频帧时间戳，无法确认画面就绪')
      }
      return
    }
    let callbackId = 0
    let active = true
    let fpsStartedAt = 0
    let fpsFrames = 0
    const frame = (_now: number, metadata: RTPVideoFrameMetadata) => {
      if (!active || runtimeRef.current !== runtime || !runtime.active) return
      const boundary = runtime.boundary
      const boundaryKey = boundary ? `${boundary.epoch}:${boundary.timestamp}` : ''
      if (boundary && validUInt32(metadata.rtpTimestamp) && rtpIsAtOrAfter(metadata.rtpTimestamp, boundary.timestamp)) {
        if (!runtime.decoded) {
          runtime.decoded = true
          setHasDecodedVideo(true)
        }
        if (runtime.presentedBoundary !== boundaryKey) {
          runtime.presentedBoundary = boundaryKey
          setHasFrame(true)
        }
      }
      if (runtime.foreground && runtime.present && boundary && runtime.acknowledgedBoundary !== boundaryKey && runtime.acknowledgingBoundary !== boundaryKey) {
        if (!validUInt32(metadata.rtpTimestamp)) {
          setSessionError('浏览器未提供 RTP 视频帧时间戳，无法安全确认画面')
          void runtime.close('failed', '浏览器未提供 RTP 视频帧时间戳，无法安全确认画面')
          return
        }
        if (rtpIsAtOrAfter(metadata.rtpTimestamp, boundary.timestamp)) {
          const sessionId = runtime.sessionId
          const epoch = boundary.epoch
          runtime.acknowledgingBoundary = boundaryKey
          void command('frame-ready', { timestamp: metadata.rtpTimestamp }).then(reply => {
            if (runtimeRef.current !== runtime || !runtime.active || !runtime.foreground || !runtime.present || runtime.sessionId !== sessionId || runtime.epoch !== epoch || runtime.boundary !== boundary) return
            if (reply.status !== 'succeeded') throw new Error(reply.message || '服务端拒绝当前视频帧确认')
            runtime.acknowledgedBoundary = boundaryKey
            setReady(true)
          }).catch(cause => {
            if (runtimeRef.current === runtime && runtime.active && runtime.sessionId === sessionId && runtime.epoch === epoch && runtime.boundary === boundary) setSessionError(errorMessage(cause))
          }).finally(() => {
            if (runtime.acknowledgingBoundary === boundaryKey) runtime.acknowledgingBoundary = ''
          })
        }
      }
      const time = metadata.expectedDisplayTime ?? performance.now()
      if (fpsStartedAt === 0) fpsStartedAt = time
      fpsFrames++
      const elapsed = time - fpsStartedAt
      if (elapsed >= 1000) {
        setFramesPerSecond(Math.round(fpsFrames * 1000 / elapsed))
        fpsStartedAt = time
        fpsFrames = 0
      }
      callbackId = video.requestVideoFrameCallback!(frame)
    }
    callbackId = video.requestVideoFrameCallback(frame)
    return () => {
      active = false
      video.cancelVideoFrameCallback?.(callbackId)
    }
  }, [command, remoteState, present])

  const sameScope = stateScope.current.deviceId === deviceId && stateScope.current.mode === mode
  const runtimeForRender = sameScope && enabled ? currentRuntime : null
  const release = useCallback(() => {
    const runtime = runtimeForRender
    if (mode !== 'control' || !ready || !runtime?.foreground || !runtime.present || runtimeRef.current !== runtime) return
    void command('release', {}).then(reply => {
      if (runtimeRef.current !== runtime || !runtime.active) return
      if (reply.status !== 'dispatched' && reply.status !== 'succeeded') setSessionError(reply.message || '释放设备输入失败')
    }).catch(cause => {
      if (runtimeRef.current === runtime && runtime.active) setSessionError(errorMessage(cause))
    })
  }, [command, mode, ready, runtimeForRender])

  const close = useCallback(() => currentRuntime?.close('closed') ?? Promise.resolve(), [currentRuntime])

  return useMemo(() => ({
    sessionId: sameScope && enabled ? sessionId : null,
    status: sameScope ? status : enabled ? 'connecting' : 'closed',
    error: [sameScope ? sessionError : '', cleanupError].filter(Boolean).join('；'),
    ready: sameScope && enabled && foreground && present && ready,
    hasFrame: sameScope && enabled && hasFrame,
    canRetain: sameScope && enabled && status === 'connected' && hasDecodedVideo,
    epoch: sameScope ? epoch : 0,
    width: sameScope ? dimensions.width : 0,
    height: sameScope ? dimensions.height : 0,
    framesPerSecond: sameScope ? framesPerSecond : 0,
    videoRef,
    command,
    release,
    suspend,
    close,
  }), [sameScope, enabled, foreground, present, sessionId, status, sessionError, cleanupError, ready, hasFrame, hasDecodedVideo, epoch, dimensions, framesPerSecond, command, release, suspend, close])
}
