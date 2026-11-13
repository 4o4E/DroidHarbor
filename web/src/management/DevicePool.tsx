import { useMemo, useState } from 'react'
import { CircleDot, LayoutGrid, MonitorSmartphone, Plus, RefreshCw, Search, Unplug } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import { DeviceMetrics } from '@/devices/DeviceMetrics'
import { deviceName, deviceNeedsVerification, deviceStatus, type Device } from '@/lib/types'

type Filter = 'all' | 'available' | 'leased' | 'offline' | 'attention'

type Props = {
  devices: Device[]
  onOpen: (device: Device) => void
  onManage: (device: Device) => void
  onAdd: () => void
  onSync: (device: Device) => void
  syncingDeviceId: string | null
}

function isOffline(device: Device) {
  return device.connectionState !== 'online' && !deviceNeedsVerification(device)
}
function needsAttention(device: Device) {
  return device.connectionState !== 'online' || device.allocationState === 'reclaiming' || device.allocationState === 'quarantined' || device.links.some(link => Boolean(link.lastError)) || Boolean(device.runtime?.issues.length)
}
function badgeVariant(device: Device): 'default' | 'secondary' | 'destructive' | 'outline' {
  if (isOffline(device) || device.allocationState === 'quarantined') return 'destructive'
  if (device.allocationState === 'leased' || device.allocationState === 'reclaiming') return 'secondary'
  return 'outline'
}

export function DevicePool({ devices, onOpen, onManage, onAdd, onSync, syncingDeviceId }: Props) {
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const counts = useMemo(() => ({
    all: devices.length,
    available: devices.filter(device => device.connectionState === 'online' && device.allocationState === 'available').length,
    leased: devices.filter(device => device.allocationState === 'leased').length,
    attention: devices.filter(needsAttention).length,
  }), [devices])
  const visible = useMemo(() => {
    const normalized = query.trim().toLocaleLowerCase()
    return devices.filter(device => {
      const matchesFilter = filter === 'all' ||
        (filter === 'available' && device.connectionState === 'online' && device.allocationState === 'available') ||
        (filter === 'leased' && device.allocationState === 'leased') ||
        (filter === 'offline' && isOffline(device)) ||
        (filter === 'attention' && needsAttention(device))
      const searchText = [device.displayName, deviceStatus(device), ...device.tags, ...device.links.map(link => `${link.kind} ${link.endpoint}`)].join(' ').toLocaleLowerCase()
      return matchesFilter && (!normalized || searchText.includes(normalized))
    })
  }, [devices, filter, query])

  return (
    <section className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div>
          <p className="mb-1 text-xs font-medium uppercase tracking-[0.16em] text-muted-foreground">Device pool</p>
          <h1 className="text-2xl font-semibold tracking-tight">设备池</h1>
          <p className="mt-2 text-sm text-muted-foreground">查看已登记设备的当前连接与分配状态。</p>
        </div>
        <Button onClick={onAdd}><Plus data-icon="inline-start" />添加设备</Button>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="全部设备" value={counts.all} icon={LayoutGrid} />
        <Stat label="可供认领" value={counts.available} icon={CircleDot} />
        <Stat label="AI 已分配" value={counts.leased} icon={MonitorSmartphone} />
        <Stat label="需要关注" value={counts.attention} icon={Unplug} />
      </div>

      <div className="flex flex-col justify-between gap-3 md:flex-row md:items-center">
        <ToggleGroup variant="outline" spacing={0} value={[filter]} onValueChange={value => { if (value[0]) setFilter(value[0] as Filter) }} aria-label="设备状态筛选">
          <ToggleGroupItem value="all">全部</ToggleGroupItem>
          <ToggleGroupItem value="available">可分配</ToggleGroupItem>
          <ToggleGroupItem value="leased">已分配</ToggleGroupItem>
          <ToggleGroupItem value="offline">离线</ToggleGroupItem>
          <ToggleGroupItem value="attention">需关注</ToggleGroupItem>
        </ToggleGroup>
        <div className="flex items-center gap-2 md:w-80"><Search className="size-4 shrink-0 text-muted-foreground" /><Input aria-label="搜索设备或标签" value={query} onChange={event => setQuery(event.target.value)} placeholder="搜索名称、链接地址或标签" /></div>
      </div>

      {visible.length > 0 ? <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {visible.map(device => <Card key={device.deviceId} size="sm" className="flex-row gap-0 py-0">
          <Button variant="ghost" className="h-auto w-40 self-stretch overflow-hidden rounded-none p-0" aria-label={`预览${deviceName(device)}的画面`} onClick={() => onOpen(device)}>
            <DevicePreview device={device} compact />
          </Button>
          <div className="flex min-w-0 flex-1 flex-col gap-4 pt-4">
            <CardHeader>
              <CardTitle className="break-words">{deviceName(device)}</CardTitle>
              {device.tags.length > 0 && <CardDescription>{device.tags.join(' · ')}</CardDescription>}
              <CardAction><Badge variant={badgeVariant(device)}>{deviceStatus(device)}</Badge></CardAction>
            </CardHeader>
            <CardContent><DeviceMetrics device={device} /></CardContent>
            <CardFooter className="mt-auto flex-wrap gap-2">
              <Button size="sm" aria-label={`预览${deviceName(device)}`} onClick={() => onOpen(device)}>预览</Button>
              <Button variant="outline" size="sm" aria-label={`管理${deviceName(device)}`} onClick={() => onManage(device)}>管理</Button>
              {needsAttention(device) && <Button variant="outline" size="sm" aria-label={`刷新${deviceName(device)}状态`} disabled={syncingDeviceId !== null} onClick={() => onSync(device)}><RefreshCw data-icon="inline-start" className={syncingDeviceId === device.deviceId ? 'animate-spin' : undefined} />{syncingDeviceId === device.deviceId ? '刷新中…' : '刷新'}</Button>}
            </CardFooter>
          </div>
        </Card>)}
      </div> : <Empty className="border">
        <EmptyHeader><EmptyMedia variant="icon"><Search /></EmptyMedia><EmptyTitle>{devices.length ? '没有匹配的设备' : '尚无已登记设备'}</EmptyTitle><EmptyDescription>{devices.length ? '试试其他名称、链接地址、标签或状态。' : '通过明确登记添加经授权的设备链路。'}</EmptyDescription></EmptyHeader>
        <EmptyContent>{devices.length ? <Button variant="outline" onClick={() => { setQuery(''); setFilter('all') }}>清除筛选</Button> : <Button onClick={onAdd}><Plus data-icon="inline-start" />添加设备</Button>}</EmptyContent>
      </Empty>}
    </section>
  )
}

function Stat({ label, value, icon: Icon }: { label: string; value: number; icon: typeof CircleDot }) {
  return <Card size="sm"><CardHeader><CardDescription>{label}</CardDescription><CardAction><Icon className="size-4 text-muted-foreground" /></CardAction><CardTitle><span className="text-2xl tabular-nums">{value}</span></CardTitle></CardHeader></Card>
}
