import { useEffect, useEffectEvent, useRef } from 'react'
import { toast } from '@/components/ui/toast'
import { ApiError, errorMessage } from './api'
import type { Device, VideoFallback } from './types'

// A polling failure keeps its first request ID until recovery or a new error.
export function useErrorNotification(title: string, error: unknown) {
  const message = error == null || error === '' ? '' : typeof error === 'string' ? error : errorMessage(error)
  const key = !message ? '' : error instanceof ApiError
    ? `${error.status}:${error.code}:${error.message}`
    : error instanceof Error ? `${error.name}:${error.message}` : message
  const addNotice = useEffectEvent(() => toast.add({ title, description: message, type: 'error', timeout: 0 }))
  useEffect(() => {
    if (!key) return
    const id = addNotice()
    return () => { toast.close(id) }
  }, [title, key])
}

// A fallback is an operational state: polling and SSE refreshes update the same
// notice; a new stage must remain visible even if the previous notice was closed.
export function useVideoFallbackNotifications(fallbacks: readonly VideoFallback[] | undefined, devices: readonly Device[] | undefined, onlyDeviceId?: string) {
  const notices = useRef(new Map<string, { id: string; signature: string }>())
  useEffect(() => {
    if (!fallbacks) return
    const current = new Set<string>()
    for (const fallback of fallbacks) {
      if (onlyDeviceId && fallback.deviceId !== onlyDeviceId) continue
      const key = `${fallback.deviceId}:${fallback.revision}`
      current.add(key)
      const name = devices?.find(device => device.deviceId === fallback.deviceId)?.displayName ?? fallback.deviceId.slice(0, 8)
      const target = `${fallback.target.maxFps} fps / ${fallback.target.bitRate / 1_000_000} Mbps`
      const applied = `${fallback.applied.maxFps} fps / ${fallback.applied.bitRate / 1_000_000} Mbps`
      const signature = `${name}:${fallback.status}:${target}:${applied}:${fallback.reason}`
      const previous = notices.current.get(key)
      if (previous?.signature === signature) continue
      if (previous) toast.close(previous.id)
      const title = fallback.status === 'trying' ? `${name} 正在回退画质` : fallback.status === 'applied' ? `${name} 已回退至 ${applied}` : `${name} 画质回退失败`
      const id = toast.add({ title, description: `目标 ${target}；${fallback.status === 'applied' ? '实际' : '尝试'} ${applied}。${fallback.reason}`, type: fallback.status === 'failed' ? 'error' : 'warning', timeout: 0 })
      notices.current.set(key, { id, signature })
    }
    for (const [key, notice] of notices.current) {
      if (!current.has(key)) { toast.close(notice.id); notices.current.delete(key) }
    }
  }, [fallbacks, devices, onlyDeviceId])
  useEffect(() => {
    const current = notices.current
    return () => { for (const notice of current.values()) toast.close(notice.id); current.clear() }
  }, [])
}
