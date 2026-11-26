import { Tooltip } from '@base-ui/react/tooltip'
import { TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { copyToComputer } from '@/lib/copyToComputer'
import { currentLinkError, type Device } from '@/lib/types'

// Unknown capabilities are not failures. Only include the public diagnostic
// reasons; never serialize the device, transport addresses or raw ADB output.
export function deviceProblemDetails(device: Device): string {
  const reasons = new Set<string>()
  const { connectionState, links } = device
  if (connectionState !== 'online') {
    if (connectionState === 'unauthorized') reasons.add('ADB 未授权。请解锁手机，确认调试授权。')
    if (links.some(link => link.connectionState === 'online')) {
      reasons.add('ADB 已连接，链路身份待核验。请核对所接手机，再刷新设备状态。')
      for (const link of links) {
        if (link.connectionState === 'online' && link.lastError) reasons.add(link.lastError)
      }
    } else if (connectionState === 'offline') {
      reasons.add('没有在线链路。请检查连接后刷新设备状态。')
      if (links.some(link => link.kind === 'wifi')) reasons.add('无线调试：确认手机与 ADB Server 同网，并核对手机显示的连接端口；配对端口与连接端口不同。')
    }
  }
  if (device.allocationState === 'quarantined') reasons.add('设备已隔离，暂不能分配。请检查链路及回收状态。')
  if (connectionState === 'online') {
    const linkError = currentLinkError(device)
    if (linkError) reasons.add(linkError)
    for (const issue of device.runtime?.issues ?? []) {
      if (issue.code === 'INPUT_INJECTION_DENIED') {
        reasons.add('Android 已拒绝输入注入。有画面不代表可以触控；小米设备请检查「USB 调试（安全设置）」。')
      } else if (issue.code === 'INITIAL_COVER_UNAVAILABLE') {
        reasons.add('未能取得预览封面。请检查手机显示状态，再打开预览。')
      } else {
        reasons.add(issue.message)
      }
    }
  }
  return reasons.size ? `${device.displayName}\n${[...reasons].join('\n')}` : ''
}

export function DeviceProblemMarker({ name, details }: { name: string; details: string }) {
  return <Tooltip.Root>
    <Tooltip.Trigger render={<Button size="icon-xs" variant="destructive" />} aria-label={`复制${name}的问题详情`} onClick={() => void copyToComputer(details)}>
      <TriangleAlert />
    </Tooltip.Trigger>
    <Tooltip.Portal><Tooltip.Positioner sideOffset={6}>
      <Tooltip.Popup className="max-w-xs whitespace-pre-line break-words rounded-md border bg-popover px-3 py-2 text-sm text-popover-foreground shadow-md">{details}</Tooltip.Popup>
    </Tooltip.Positioner></Tooltip.Portal>
  </Tooltip.Root>
}
