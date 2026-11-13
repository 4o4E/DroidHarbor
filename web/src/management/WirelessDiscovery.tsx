import { useState, type FormEvent } from 'react'
import { Wifi } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyTitle } from '@/components/ui/empty'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { toast } from '@/components/ui/toast'
import { api, errorMessage } from '@/lib/api'
import type { DiscoveredDevice, WirelessService } from '@/lib/types'

type Props = {
  disabled: boolean
  onBusyChange: (active: boolean) => void
  onConnected: (devices: DiscoveredDevice[], endpoint: string) => void
}
const serviceKey = (service: WirelessService) => `${service.type}:${service.name}:${service.endpoint}`
const isPairing = (service: WirelessService) => service.type === '_adb-tls-pairing._tcp'

export function WirelessDiscovery({ disabled, onBusyChange, onConnected }: Props) {
  const [services, setServices] = useState<WirelessService[] | null>(null)
  const [pending, setPending] = useState('')
  const [pairing, setPairing] = useState<WirelessService | null>(null)
  const [pairingCode, setPairingCode] = useState('')

  function busy(action: string) {
    setPending(action)
    onBusyChange(Boolean(action))
  }

  async function scan() {
    if (disabled || pending) return
    busy('scan')
    setPairing(null)
    setPairingCode('')
    try {
      const result = await api<{ services: WirelessService[] }>('/device-discovery/wireless')
      setServices(result.services)
    } catch (error) {
      // A failed scan cannot leave old endpoints looking freshly discovered.
      setServices(null)
      toast.add({ title: '无法扫描无线设备', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      busy('')
    }
  }

  async function connect(service: WirelessService) {
    if (disabled || pending) return
    busy(serviceKey(service))
    try {
      const result = await api<{ devices: DiscoveredDevice[] }>('/device-discovery/wireless/connect', { method: 'POST', body: service })
      onConnected(result.devices, service.endpoint)
      setServices(current => current?.filter(item => serviceKey(item) !== serviceKey(service)) ?? null)
      toast.add({ title: '无线设备已连接', description: '可登记并核验，或一键添加所有设备。', type: 'success' })
    } catch (error) {
      toast.add({ title: '无线连接未完成', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      busy('')
    }
  }

  async function pair(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const code = pairingCode.trim()
    if (!pairing || !code || disabled || pending) return
    const selected = pairing
    busy('pair')
    setPairingCode('')
    let completed = false
    try {
      await api<void>('/device-discovery/wireless/pair', { method: 'POST', body: { ...selected, pairingCode: code } })
      completed = true
      setPairing(null)
      toast.add({ title: '无线配对已完成', description: '请选择连接服务，再登记设备。', type: 'success' })
      const result = await api<{ services: WirelessService[] }>('/device-discovery/wireless')
      setServices(result.services)
    } catch (error) {
      if (completed) setServices(null)
      toast.add({ title: completed ? '配对已完成，扫描刷新失败' : '无线配对未完成', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      busy('')
    }
  }

  return <div className="flex flex-col gap-3">
    <Button type="button" variant="outline" disabled={disabled || Boolean(pending)} onClick={() => void scan()}>
      <Wifi data-icon="inline-start" />{pending === 'scan' ? '正在扫描…' : '扫描局域网无线设备'}
    </Button>
    {services !== null && (services.length ? <div className="flex flex-col gap-3">
      {services.map(service => <div key={serviceKey(service)} className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2"><p className="break-all text-sm">{service.endpoint}</p><Badge variant="outline">{isPairing(service) ? '配对服务' : '连接服务'}</Badge></div>
          <p className="break-all text-xs text-muted-foreground">{service.name}</p>
        </div>
        <Button type="button" size="sm" variant="outline" disabled={disabled || Boolean(pending)} onClick={() => {
          if (isPairing(service)) { setPairing(service); setPairingCode('') }
          else void connect(service)
        }}>{pending === serviceKey(service) ? '正在连接…' : isPairing(service) ? '输入配对码' : '连接'}</Button>
      </div>)}
    </div> : <Empty><EmptyHeader><EmptyTitle>未发现待添加的无线服务</EmptyTitle><EmptyDescription>在手机打开无线调试；首次配对时打开配对码页面，然后重新扫描。</EmptyDescription></EmptyHeader></Empty>)}
    {pairing && <form onSubmit={event => void pair(event)}><FieldGroup>
      <Field data-disabled={disabled || Boolean(pending)}><FieldLabel htmlFor="wireless-pairing-code">临时配对码</FieldLabel><Input id="wireless-pairing-code" type="password" inputMode="numeric" autoComplete="off" value={pairingCode} onChange={event => setPairingCode(event.target.value)} required disabled={disabled || Boolean(pending)} /><FieldDescription>{pairing.endpoint}</FieldDescription></Field>
      <div className="flex gap-2"><Button type="submit" size="sm" disabled={disabled || Boolean(pending) || !pairingCode.trim()}>{pending === 'pair' ? '正在配对…' : '配对'}</Button><Button type="button" size="sm" variant="outline" disabled={disabled || Boolean(pending)} onClick={() => { setPairing(null); setPairingCode('') }}>取消配对</Button></div>
    </FieldGroup></form>}
  </div>
}
