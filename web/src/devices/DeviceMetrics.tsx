import { useEffect, useState } from 'react'
import { Battery, Cpu, MemoryStick, Plug, Thermometer } from 'lucide-react'
import { cn } from 'cn'
import type { Device } from '@/lib/types'
import { useErrorNotification } from '@/lib/notifications'

export function useDeviceRuntimeNotification(device: Device) {
  const issues = device.connectionState === 'online'
    ? [...(device.runtime?.issues ?? []).map(issue => issue.message), ...device.links.map(link => link.lastError).filter(Boolean)]
    : []
  useErrorNotification(`${device.displayName}状态初始化失败`, issues.join('；'))
}

// Only the initial connection waits for the online controller. A later power
// fault is reported without tearing down another healthy viewing connection.
export function useInitializedDevice(device: Device) {
  const [acceptedDevice, setAcceptedDevice] = useState('')
  const online = device.connectionState === 'online'
  const powerActive = device.runtime?.powerState === 'active'
  useEffect(() => {
    if (!online) setAcceptedDevice('')
    else if (powerActive) setAcceptedDevice(device.deviceId)
  }, [online, powerActive, device.deviceId])
  return online && (powerActive || acceptedDevice === device.deviceId)
}

const chargeLabels: Record<string, string> = {
  unknown: '状态未知', charging: '充电中', discharging: '放电中', 'not-charging': '未充电', full: '已充满',
}

export function DeviceMetrics({ device, compact = false }: { device: Device; compact?: boolean }) {
  useDeviceRuntimeNotification(device)
  const metrics = device.connectionState === 'online' ? device.runtime?.metrics : undefined
  const pending = device.connectionState === 'online' ? '初始化中' : '—'
  const missing = metrics ? '不可用' : pending
  const cpuFailure = device.runtime?.issues.some(issue => issue.stage === 'cpu')
  const cpu = metrics?.cpuPercent == null ? metrics && !cpuFailure ? '采样中' : missing : `${metrics.cpuPercent.toFixed(1)}%`
  const memory = metrics?.memoryUsedBytes == null || metrics.memoryTotalBytes == null ? missing
    : `${(metrics.memoryUsedBytes / 2 ** 30).toFixed(compact ? 1 : 2)}/${(metrics.memoryTotalBytes / 2 ** 30).toFixed(compact ? 1 : 2)} GiB`
  return <dl className={cn('device-metrics', compact && 'device-metrics-compact')} aria-label={`${device.displayName}基本指标`} title={metrics ? `采样于 ${new Date(metrics.observedAt).toLocaleString('zh-CN')}` : undefined}>
    <div><dt><Cpu />CPU</dt><dd>{cpu}</dd></div>
    <div><dt><MemoryStick />内存</dt><dd>{memory}</dd></div>
    <div><dt><Thermometer />电池温度</dt><dd>{metrics?.batteryTemperatureC == null ? missing : `${metrics.batteryTemperatureC.toFixed(1)} °C`}</dd></div>
    <div><dt><Battery />电量</dt><dd>{metrics?.batteryPercent == null ? missing : <>{Math.round(metrics.batteryPercent)}%<span className="device-charge-state">{metrics.powerConnected && <Plug aria-label="已接电源" />}{chargeLabels[metrics.chargingStatus ?? 'unknown']}</span></>}</dd></div>
  </dl>
}
