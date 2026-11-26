export type SessionMode = 'readonly' | 'control'
export type AudioRoute = 'muted' | 'device' | 'browser'
export interface SystemPreferences { audioDefault: AudioRoute; windowScales: number[]; audioGainMax: number; revision: number }
export type ConnectionState = 'online' | 'offline' | 'unauthorized'
export type AllocationState = 'available' | 'leased' | 'reclaiming' | 'quarantined' | 'maintenance'
export type DiscoveredDevice = { serial: string; state: string; transportId: number; model?: string; product?: string }
export type WirelessService = { name: string; type: '_adb._tcp' | '_adb-tls-connect._tcp' | '_adb-tls-pairing._tcp'; endpoint: string }

export interface DeviceLink {
  linkId: string
  kind: 'usb' | 'wifi'
  endpoint: string
  connectionState: ConnectionState
  identityVerified: boolean
  lastError?: string
}
export interface DisplayGeometry { width: number; height: number; observedAt: string }
export interface DeviceDisplayInfo {
  screen?: DisplayGeometry & { rotation: number }
  video?: DisplayGeometry
}
export interface DeviceRuntimeIssue { stage: string; code: string; message: string }
export interface DeviceMetricsSnapshot {
  observedAt: string
  cpuPercent: number | null
  memoryUsedBytes: number | null
  memoryTotalBytes: number | null
  batteryTemperatureC: number | null
  batteryPercent: number | null
  powerConnected: boolean | null
  chargingStatus: 'unknown' | 'charging' | 'discharging' | 'not-charging' | 'full' | null
}
export interface DeviceRuntimeInfo {
  linkId: string
  linkGeneration: number
  powerState: 'starting' | 'active' | 'failed'
  coverState: 'pending' | 'ready' | 'failed'
  metrics?: DeviceMetricsSnapshot
  issues: DeviceRuntimeIssue[]
}
export interface Device {
  deviceId: string
  displayName: string
  tags: string[]
  connectionState: ConnectionState
  allocationState: AllocationState
  version: string
  maintenanceRequested: boolean
  leaseGeneration: number
  links: DeviceLink[]
  displayInfo?: DeviceDisplayInfo
  runtime?: DeviceRuntimeInfo
}
export interface Caller {
  id: string
  name: string
  createdAt: string
  revokedAt: string | null
}
export interface AdminSession {
  sessionId: string
  csrfToken: string
  expiresAt?: string
}
export interface VideoQuality { maxFps: number; bitRate: number }
export interface VideoFallback {
  deviceId: string
  revision: number
  target: VideoQuality
  applied: VideoQuality
  status: 'trying' | 'applied' | 'failed'
  reason: string
}
export interface GlobalVideoSnapshot {
  requested: VideoQuality
  revision: number
  fallbacks: VideoFallback[]
}
export const deviceName = (device: Device) => device.displayName

// Online means the platform has a verified working route. Only its current
// runtime link can contribute a link error to device-level diagnostics.
export function currentLinkError(device: Device) {
  if (device.connectionState !== 'online' || !device.runtime) return undefined
  const { linkId } = device.runtime
  return device.links.find(link => link.linkId === linkId)?.lastError
}

export function deviceDisplaySize(device: Device): DisplayGeometry | null {
  const { screen, video } = device.displayInfo ?? {}
  if (screen && video) return Date.parse(video.observedAt) >= Date.parse(screen.observedAt) ? video : screen
  return video ?? screen ?? null
}
const allocationLabels: Record<AllocationState, string> = { available: '未分配', leased: '已分配', reclaiming: '回收中', quarantined: '已隔离', maintenance: '不可分配' }
export function deviceNeedsVerification(device: Device) {
  return device.connectionState === 'offline' && device.links.some(link => link.connectionState === 'online' && !link.identityVerified)
}
export function deviceStatus(device: Device) {
  if (deviceNeedsVerification(device)) return '待核验'
  if (device.connectionState === 'offline') return '离线'
  if (device.connectionState === 'unauthorized') return '未授权'
  return allocationLabels[device.allocationState]
}
