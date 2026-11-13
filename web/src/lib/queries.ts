import { useEffect, useState, useSyncExternalStore } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, errorMessage } from './api'
import { useAuth } from './auth'
import type { AdminSession, Caller, Device, VideoSnapshot } from './types'

let snapshotsReady = false
const readinessListeners = new Set<() => void>()
function setSnapshotsReady(value: boolean) {
  if (snapshotsReady === value) return
  snapshotsReady = value
  for (const listener of readinessListeners) listener()
}
function useSnapshotsReady() {
  return useSyncExternalStore(listener => {
    readinessListeners.add(listener)
    return () => { readinessListeners.delete(listener) }
  }, () => snapshotsReady)
}

export function useDevices() {
  const { session } = useAuth()
  const ready = useSnapshotsReady()
  const query = useQuery({
    queryKey: ['devices'],
    enabled: !!session && ready,
    queryFn: async ({ signal }) => {
      const result = await api<{ devices: Device[]; discoveryReady: boolean }>('/devices', { signal })
      return { ...result, devices: result.devices.map(device => ({ ...device, links: device.links ?? [] })) }
    },
  })
  return { ...query, data: query.data?.devices, discoveryReady: query.data?.discoveryReady ?? false }
}
export function useCallers() {
  const { session } = useAuth()
  const ready = useSnapshotsReady()
  return useQuery({ queryKey: ['callers'], enabled: !!session && ready, queryFn: async ({ signal }) => (await api<{ callers: Caller[] }>('/callers', { signal })).callers })
}
export function useVideoSettings(deviceId: string) {
  const { session } = useAuth()
  const ready = useSnapshotsReady()
  return useQuery({
    queryKey: ['video-settings', deviceId],
    enabled: !!session && ready && !!deviceId,
    queryFn: ({ signal }) => api<VideoSnapshot>('/devices/' + encodeURIComponent(deviceId) + '/video-settings', { signal }),
    refetchInterval: 2000,
    refetchIntervalInBackground: false,
  })
}

// Events invalidate snapshots only. Reopening this read-only stream never replays a device action.
export function usePlatformEvents() {
  const { session } = useAuth()
  const sessionId = session?.sessionId
  const client = useQueryClient()
  const [status, setStatus] = useState<'connecting' | 'connected' | 'disconnected'>('connecting')
  const [error, setError] = useState('')
  useEffect(() => {
    if (!sessionId) return
    let active = true
    let bootId = ''
    let lastVersion = 0
    let checkingSession = false
    let source: EventSource | null = null
    const controller = new AbortController()
    setSnapshotsReady(false)
    const refresh = () => {
      void client.invalidateQueries({ queryKey: ['devices'] })
      void client.invalidateQueries({ queryKey: ['callers'] })
      void client.invalidateQueries({ queryKey: ['video-settings'] })
    }
    const connect = () => {
      source?.close()
      setStatus('connecting')
      const stream = new EventSource('/api/v1/events')
      source = stream
      stream.addEventListener('change', event => {
        if (!active || source !== stream) return
        try {
          const change = JSON.parse((event as MessageEvent<string>).data) as { bootId: string; entityVersion: number; entityType: string; entityId: string }
          if (bootId === change.bootId && change.entityVersion < lastVersion && change.entityType !== 'resync') return
          bootId = change.bootId
          lastVersion = change.entityVersion
          setSnapshotsReady(true)
          setStatus('connected')
          setError('')
          if (change.entityType === 'device-runtime') void client.invalidateQueries({ queryKey: ['devices'] })
          else refresh()
        } catch (failure) {
          stream.close()
          setSnapshotsReady(false)
          setStatus('disconnected')
          setError('平台状态通知格式无效，请刷新页面。' + errorMessage(failure))
        }
      })
      stream.onopen = () => {
        if (!active || source !== stream) return
        setStatus('connected')
        setError('')
        setSnapshotsReady(true)
        refresh()
      }
      stream.onerror = () => {
        if (!active || source !== stream) return
        setStatus('disconnected')
        setSnapshotsReady(false)
        setError('状态连接已断开，正在重新同步。')
        if (checkingSession) return
        checkingSession = true
        void api<AdminSession>('/admin/sessions/current', { signal: controller.signal }).catch(failure => {
          if (active && !controller.signal.aborted) setError(errorMessage(failure))
        }).finally(() => { checkingSession = false })
      }
    }
    // HTTP 离线时，不能将尚未关闭的旧 SSE 当作可用状态；恢复后重新订阅并读取权威快照。
    const offline = () => {
      source?.close()
      source = null
      setSnapshotsReady(false)
      setStatus('disconnected')
      setError('网络已断开，恢复连接后将重新读取设备状态。')
    }
    const online = () => { if (active) connect() }
    const resume = () => { if (navigator.onLine && document.visibilityState === 'visible') refresh() }
    window.addEventListener('offline', offline)
    window.addEventListener('online', online)
    document.addEventListener('visibilitychange', resume)
    if (navigator.onLine) connect()
    else offline()
    return () => {
      active = false
      controller.abort()
      source?.close()
      window.removeEventListener('offline', offline)
      window.removeEventListener('online', online)
      document.removeEventListener('visibilitychange', resume)
      setSnapshotsReady(false)
    }
  }, [sessionId, client])
  return { status, error }
}
