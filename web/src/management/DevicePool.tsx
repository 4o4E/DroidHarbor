import { useMemo, useState } from 'react'
import { Cable, CircleDot, LayoutGrid, MonitorSmartphone, Plus, Search, Unplug, Wifi } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Input } from '@/components/ui/input'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { DevicePreview } from '@/devices/DevicePreview'
import { deviceName, deviceStatus, type Device } from '@/lib/types'

type Filter = 'all' | 'available' | 'leased' | 'offline' | 'attention'

type Props = {
  devices: Device[]
  onOpen: (device: Device) => void
  onManage: (device: Device) => void
  onAdd: () => void
}

function isOffline(device: Device) {
  return device.connectionState !== 'online'
}
function needsAttention(device: Device) {
  return isOffline(device) || device.allocationState === 'reclaiming' || device.allocationState === 'quarantined'
}
function badgeVariant(device: Device): 'default' | 'secondary' | 'destructive' | 'outline' {
  if (isOffline(device) || device.allocationState === 'quarantined') return 'destructive'
  if (device.allocationState === 'leased' || device.allocationState === 'reclaiming') return 'secondary'
  return 'outline'
}

export function DevicePool({ devices, onOpen, onManage, onAdd }: Props) {
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
        <Stat label="AI 使用中" value={counts.leased} icon={MonitorSmartphone} />
        <Stat label="需要关注" value={counts.attention} icon={Unplug} />
      </div>

      <div className="flex flex-col justify-between gap-3 md:flex-row md:items-center">
        <ToggleGroup variant="outline" spacing={0} value={[filter]} onValueChange={value => { if (value[0]) setFilter(value[0] as Filter) }} aria-label="设备状态筛选">
          <ToggleGroupItem value="all">全部</ToggleGroupItem>
          <ToggleGroupItem value="available">空闲</ToggleGroupItem>
          <ToggleGroupItem value="leased">使用中</ToggleGroupItem>
          <ToggleGroupItem value="offline">离线</ToggleGroupItem>
          <ToggleGroupItem value="attention">需关注</ToggleGroupItem>
        </ToggleGroup>
        <div className="flex items-center gap-2 md:w-80"><Search className="size-4 shrink-0 text-muted-foreground" /><Input aria-label="搜索设备或标签" value={query} onChange={event => setQuery(event.target.value)} placeholder="搜索名称、链接地址或标签" /></div>
      </div>

      {visible.length > 0 ? <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
        {visible.map(device => <Card key={device.deviceId} className="relative">
          <Button variant="link" className="absolute inset-0 h-full w-full cursor-pointer rounded-xl" aria-label={`打开${deviceName(device)}的工作台`} onClick={() => onOpen(device)} />
          <CardHeader>
            <CardTitle>{deviceName(device)}</CardTitle>
            <CardDescription>{device.tags.length ? device.tags.join(' · ') : '未设置标签'}</CardDescription>
            <CardAction><Badge variant={badgeVariant(device)}>{deviceStatus(device)}</Badge></CardAction>
          </CardHeader>
          <CardContent className="flex flex-col gap-5">
            <DevicePreview device={device} />
            <div className="flex items-center justify-between gap-2 text-sm">
              <span className="text-muted-foreground">接入链路</span>
              <span className="flex items-center gap-1.5">{device.links.length === 0 ? '尚未登记' : device.links.map(link => <span key={link.linkId} className="inline-flex items-center gap-1"><LinkIcon kind={link.kind} />{link.kind.toUpperCase()}</span>)}</span>
            </div>
            <div className="flex min-h-5 flex-wrap gap-1.5">{device.tags.map(tag => <Badge key={tag} variant="outline">{tag}</Badge>)}</div>
          </CardContent>
          <CardFooter><Button className="relative" variant="ghost" size="sm" aria-label={`管理${deviceName(device)}`} onClick={() => onManage(device)}>管理</Button></CardFooter>
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

function LinkIcon({ kind }: { kind: Device['links'][number]['kind'] }) {
  return kind === 'usb' ? <Cable className="size-4" /> : <Wifi className="size-4" />
}
