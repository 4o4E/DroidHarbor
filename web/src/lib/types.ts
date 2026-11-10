export type SessionMode = 'readonly' | 'control'
export type ConnectionState = 'online' | 'offline' | 'unauthorized'
export type AllocationState = 'available' | 'leased' | 'reclaiming' | 'quarantined' | 'maintenance'

export interface DeviceLink {
  linkId: string
  kind: 'usb' | 'wifi'
  endpoint: string
  connectionState: ConnectionState
  identityVerified: boolean
  lastError?: string
}
export interface Device {
  deviceId: string
  displayName: string
  tags: string[]
  connectionState: ConnectionState
  allocationState: AllocationState
  version: string
  maintenanceRequested: boolean
  links: DeviceLink[]
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
export interface VideoParameters { maxSize: number; maxFps: number; bitRate: number }
export interface VideoSnapshot {
  requested: VideoParameters
  effective: { width: number; height: number; framesPerSecond: number; encodedBitsPerSecond: number; revision: number } | null
  status: 'idle' | 'pending' | 'starting' | 'running' | 'reconfiguring' | 'stopping' | 'failed'
  revision: number
  captureEpoch: number
  errorCode?: string
}
export interface Transfer {
  transferId: string
  fileName: string
  purpose: 'file' | 'apk'
  targetPath?: string
  state: 'uploading' | 'transferring' | 'installing' | 'succeeded' | 'failed' | 'cancelled' | 'unknown'
  bytesReceived: number
  bytesTransferred: number
  size: number
  overwrite: boolean
  exitCode?: number
  stdout?: string
  stderr?: string
  result?: string
  errorCode?: string
  error?: string
  createdAt: string
  updatedAt: string
}
export const deviceName = (device: Device) => device.displayName
const allocationLabels: Record<AllocationState, string> = { available: '空闲', leased: 'AI 使用中', reclaiming: '回收中', quarantined: '已隔离', maintenance: '维护中' }
export function deviceNeedsVerification(device: Device) {
  return device.connectionState === 'offline' && device.links.some(link => link.connectionState === 'online' && !link.identityVerified)
}
export function deviceStatus(device: Device) {
  if (deviceNeedsVerification(device)) return '待核验'
  if (device.connectionState === 'offline') return '离线'
  if (device.connectionState === 'unauthorized') return '未授权'
  return allocationLabels[device.allocationState]
}
