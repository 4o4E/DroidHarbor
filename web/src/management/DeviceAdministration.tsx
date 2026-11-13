import { useEffect, useState, type FormEvent } from 'react'
import { Cable, RefreshCw, Wifi } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Separator } from '@/components/ui/separator'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toast } from '@/components/ui/toast'
import { api, ApiError, errorMessage } from '@/lib/api'
import type { Device, DeviceLink, DiscoveredDevice } from '@/lib/types'
import { WirelessDiscovery } from './WirelessDiscovery'
type DiscoveredResponse = { devices: DiscoveredDevice[] }
type LinkKind = DeviceLink['kind']
type Props = {
  open: boolean
  onOpenChange: (open: boolean) => void
  device: Device | null
  onChanged: () => Promise<void>
  addDeviceOpen: boolean
  onAddDeviceOpenChange: (open: boolean) => void
}

export function DeviceAdministration({ open, onOpenChange, device, onChanged, addDeviceOpen, onAddDeviceOpenChange }: Props) {
  const [displayName, setDisplayName] = useState('')
  const [tagsText, setTagsText] = useState('')
  const [busy, setBusy] = useState('')
  const [maintenanceConfirmation, setMaintenanceConfirmation] = useState<{ generation: number } | null>(null)

  useEffect(() => {
    if (!open || !device) return
    setDisplayName(device.displayName)
    setTagsText(device.tags.join(', '))
    setMaintenanceConfirmation(null)
  }, [open, device?.deviceId, device?.version])

  async function perform(action: string, request: () => Promise<void>, success: string) {
    if (busy) return
    setBusy(action)
    try {
      await request()
      await onChanged()
      toast.add({ title: '操作完成', description: success, type: 'success' })
    } catch (error) {
      const unknown = error instanceof ApiError && (error.code === 'RESULT_UNKNOWN' || error.code === 'ADB_RESULT_UNKNOWN')
      toast.add({
        title: unknown ? '操作结果未确认' : '操作未完成',
        description: unknown ? `${errorMessage(error)} 请重新读取设备状态。` : errorMessage(error),
        type: 'error', timeout: 0,
        actionProps: error instanceof ApiError && (error.status === 409 || unknown) ? { children: '重新读取', onClick: () => void refreshAfterConflict() } : undefined,
      })
    } finally {
      setBusy('')
    }
  }

  async function saveDetails(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!device || busy) return
    const name = displayName.trim()
    const tags = [...new Set(tagsText.split(',').map(tag => tag.trim()).filter(Boolean))]
    if (!name || name.length > 120) {
      toast.add({ title: '请检查设备名称', description: '设备名称去首尾空白后须为 1–120 个字符。', type: 'warning' })
      return
    }
    const body: { version: string; displayName?: string; tags?: string[] } = { version: device.version }
    if (name !== device.displayName) body.displayName = name
    if (tags.length !== device.tags.length || tags.some((tag, index) => tag !== device.tags[index])) body.tags = tags
    if (body.displayName === undefined && body.tags === undefined) {
      toast.add({ title: '设备名称和标签没有变化', type: 'info' })
      return
    }
    await perform('details', () => api<void>('/devices/' + encodeURIComponent(device.deviceId), { method: 'PATCH', body }), '设备名称/标签已提交。')
  }

  async function requestMaintenance(requested: boolean, confirmedGeneration?: number) {
    if (!device || busy) return
    if (requested && device.allocationState === 'leased' && confirmedGeneration === undefined) {
      setMaintenanceConfirmation({ generation: device.leaseGeneration })
      return
    }
    setBusy('maintenance')
    try {
      await api<void>('/devices/' + encodeURIComponent(device.deviceId), {
        method: 'PATCH', body: { version: device.version, maintenanceRequested: requested, confirmedLeaseGeneration: confirmedGeneration },
      })
      setMaintenanceConfirmation(null)
      await onChanged()
      toast.add({ title: requested ? '已关闭分配' : '已允许分配', type: 'success' })
    } catch (error) {
      if (error instanceof ApiError && error.code === 'ALLOCATION_CONFIRMATION_REQUIRED') {
        try {
          const latest = await api<{ device: Device }>('/devices/' + encodeURIComponent(device.deviceId))
          setMaintenanceConfirmation({ generation: latest.device.leaseGeneration })
          await onChanged()
          if (confirmedGeneration !== undefined) toast.add({ title: 'AI 使用状态已变化，请重新确认', type: 'warning' })
        } catch (refreshError) {
          setMaintenanceConfirmation(null)
          toast.add({ title: '无法读取最新设备状态', description: errorMessage(refreshError), type: 'error', timeout: 0 })
        }
      } else {
        toast.add({ title: '分配设置未完成', description: errorMessage(error), type: 'error', timeout: 0, actionProps: { children: '刷新状态', onClick: () => void refreshAfterConflict() } })
      }
    } finally {
      setBusy('')
    }
  }

  async function refreshAfterConflict() {
    try {
      await onChanged()
      toast.add({ title: '设备状态已重新读取', type: 'success' })
    } catch (error) {
      toast.add({ title: '无法重新读取设备', description: errorMessage(error), type: 'error', timeout: 0 })
    }
  }

  return <>
    <CreateDeviceDialog open={addDeviceOpen} onOpenChange={onAddDeviceOpenChange} onCreated={onChanged} />
    <Dialog open={open && !!device} onOpenChange={next => { if (!busy) onOpenChange(next) }}>
      <DialogContent className="max-h-[90vh] overflow-y-auto" showCloseButton={!busy}>
        <DialogHeader><DialogTitle>管理设备</DialogTitle><DialogDescription>{device?.displayName ?? ''}</DialogDescription></DialogHeader>
        {device && <div className="flex flex-col gap-5">

          <Card size="sm">
            <CardHeader><CardTitle>设备信息</CardTitle></CardHeader>
            <CardContent>
              <form onSubmit={saveDetails}><FieldGroup>
                <Field><FieldLabel htmlFor="managed-device-name">设备名称</FieldLabel><Input id="managed-device-name" value={displayName} onChange={event => setDisplayName(event.target.value)} maxLength={120} required /></Field>
                <Field><FieldLabel htmlFor="managed-device-tags">标签</FieldLabel><Input id="managed-device-tags" value={tagsText} onChange={event => setTagsText(event.target.value)} placeholder="以逗号分隔" /></Field>
                <Button type="submit" disabled={Boolean(busy)}>{busy === 'details' ? '正在保存…' : '保存名称与标签'}</Button>
              </FieldGroup></form>
              <Separator className="my-5" />
              <Field data-disabled={Boolean(busy)}>
                <FieldLabel>AI 分配</FieldLabel>
                <ToggleGroup variant="outline" spacing={0} value={[device.maintenanceRequested ? 'disabled' : 'enabled']} disabled={Boolean(busy)} aria-label="AI 分配" onValueChange={value => {
                  if (value[0] && (value[0] === 'disabled') !== device.maintenanceRequested) void requestMaintenance(value[0] === 'disabled')
                }}>
                  <ToggleGroupItem value="disabled">不可分配</ToggleGroupItem>
                  <ToggleGroupItem value="enabled">可分配</ToggleGroupItem>
                </ToggleGroup>
              </Field>
            </CardContent>
          </Card>
        </div>}
        <DialogFooter><Button variant="outline" disabled={Boolean(busy)} onClick={() => onOpenChange(false)}>关闭</Button></DialogFooter>
      </DialogContent>
    </Dialog>
    <Dialog open={open && maintenanceConfirmation !== null} onOpenChange={next => { if (!next && !busy) setMaintenanceConfirmation(null) }}>
      <DialogContent showCloseButton={!busy}>
        <DialogHeader>
          <DialogTitle>关闭分配并结束 AI 任务？</DialogTitle>
          <DialogDescription>{device?.displayName}正在被 AI 使用，关闭分配会结束当前 AI 任务。</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={Boolean(busy)} onClick={() => setMaintenanceConfirmation(null)}>取消</Button>
          <Button variant="destructive" disabled={Boolean(busy)} onClick={() => { if (maintenanceConfirmation) void requestMaintenance(true, maintenanceConfirmation.generation) }}>{busy === 'maintenance' ? '正在关闭…' : '结束任务并关闭分配'}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </>
}

function CreateDeviceDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: () => Promise<void> }) {
  const [displayName, setDisplayName] = useState('')
  const [kind, setKind] = useState<LinkKind>('usb')
  const [endpoint, setEndpoint] = useState('')
  const [pairingEndpoint, setPairingEndpoint] = useState('')
  const [pairingCode, setPairingCode] = useState('')
  const [discovery, setDiscovery] = useState<DiscoveredDevice[] | null>(null)
  const [registered, setRegistered] = useState<{ deviceId: string; linkId: string; stage: 'pair' | 'refresh' | 'verify' } | null>(null)
  const [pending, setPending] = useState('')
  const [wirelessGeneration, setWirelessGeneration] = useState(0)

  useEffect(() => {
    if (!open) return
    setDisplayName('')
    setKind('usb')
    setEndpoint('')
    setPairingEndpoint('')
    setPairingCode('')
    setDiscovery(null)
    setRegistered(null)
  }, [open])

  async function readDiscovery() {
    setPending('discovery')
    try {
      const response = await api<DiscoveredResponse>('/device-discovery')
      setDiscovery(response.devices)
    } catch (error) {
      toast.add({ title: '无法读取发现快照', description: errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      setPending('')
    }
  }

  async function registerAll() {
    if (pending || registered) return
    setPending('all')
    const failures: string[] = []
    const processed = new Set<string>()
    let unverified = 0
    try {
      const response = await api<DiscoveredResponse>('/device-discovery')
      setDiscovery(response.devices)
      for (const found of response.devices) {
        try {
          const result = await api<{ deviceId: string; verified: boolean }>('/devices/discovered', { method: 'POST', body: { endpoint: found.serial } })
          processed.add(result.deviceId)
          if (!result.verified) unverified++
          setDiscovery(current => current?.filter(item => item.serial !== found.serial) ?? null)
        } catch (error) {
          failures.push(`${found.serial}：${errorMessage(error)}`)
        }
      }
      await onCreated()
      if (processed.size) setWirelessGeneration(current => current + 1)
      const latest = await api<DiscoveredResponse>('/device-discovery')
      setDiscovery(latest.devices)
      if (response.devices.some(item => item.serial === endpoint) && !latest.devices.some(item => item.serial === endpoint)) {
        setDisplayName('')
        setEndpoint('')
      }
      if (processed.size) toast.add({ title: `已接入 ${processed.size} 台设备`, description: unverified ? `${unverified} 条链路待手机授权或身份核验。` : undefined, type: unverified ? 'warning' : 'success' })
      else if (!failures.length) toast.add({ title: '没有待添加的设备', type: 'info' })
      if (failures.length) toast.add({ title: `${failures.length} 项接入未完成`, description: failures.join('\n'), type: 'error', timeout: 0 })
    } catch (error) {
      toast.add({ title: '批量接入未完成', description: [...failures, errorMessage(error)].join('\n'), type: 'error', timeout: 0 })
    } finally { setPending('') }
  }

  async function register(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const name = displayName.trim()
    const address = endpoint.trim()
    if (!name || !address || pending) return
    const wantsPairing = kind === 'wifi' && (!registered || registered.stage === 'pair') && Boolean(pairingEndpoint.trim() || pairingCode.trim() || registered?.stage === 'pair')
    if (wantsPairing && (!pairingEndpoint.trim() || !pairingCode.trim())) {
      toast.add({ title: '请填写配对地址和配对码', type: 'warning' })
      return
    }
    setPending(registered?.stage ?? 'register')
    let created = registered
    let failureMessage = ''
    let verified = false
    try {
      if (!created) {
        const found = discovery?.find(item => item.serial === address)
        if (found) {
          const result = await api<{ deviceId: string; linkId: string; verified: boolean }>('/devices/discovered', { method: 'POST', body: { endpoint: address, displayName: name } })
          created = { ...result, stage: wantsPairing ? 'pair' : 'verify' }
          verified = result.verified
        } else {
          const result = await api<{ deviceId: string; linkId: string }>('/devices', { method: 'POST', body: { displayName: name, kind, endpoint: address } })
          created = { ...result, stage: wantsPairing ? 'pair' : kind === 'wifi' ? 'refresh' : 'verify' }
        }
        setRegistered(created)
        setDiscovery(current => current?.filter(item => item.serial !== address) ?? null)
      }
      const devicePath = '/devices/' + encodeURIComponent(created.deviceId)
      const linkPath = devicePath + '/links/' + encodeURIComponent(created.linkId)
      if (created.stage === 'pair') {
        setPending('pair')
        const code = pairingCode.trim()
        setPairingCode('')
        await api<void>(linkPath + '/pair', { method: 'POST', body: { pairingEndpoint: pairingEndpoint.trim(), pairingCode: code } })
        created = { ...created, stage: 'refresh' }
        setRegistered(created)
      }
      setPending(created.stage)
      if (created.stage === 'refresh') await api<void>(devicePath + '/refresh', { method: 'POST' })
      else if (!verified) await api<void>(linkPath + '/verify', { method: 'POST' })
    } catch (error) {
      const unknown = error instanceof ApiError && (error.code === 'RESULT_UNKNOWN' || error.code === 'ADB_RESULT_UNKNOWN')
      failureMessage = created ? `设备已添加，接入尚未完成：${errorMessage(error)} 可继续添加，无需重复登记。` : errorMessage(error)
      if (unknown) failureMessage += ' 请先核对设备状态，操作结果尚未确认。'
    }
    setPending('refresh')
    try {
      await onCreated()
    } catch (error) {
      failureMessage += `${failureMessage ? ' ' : ''}设备列表刷新失败：${errorMessage(error)}`
    }
    if (failureMessage) toast.add({ title: '设备接入未完成', description: failureMessage, type: 'error', timeout: 0 })
    else {
      toast.add({ title: '设备已登记并完成身份核验', type: 'success' })
      onOpenChange(false)
    }
    setPending('')
  }

  function selectDiscovered(device: DiscoveredDevice) {
    setKind(/:\d+$|\._adb(?:-tls-connect)?\._tcp\.?$/.test(device.serial) ? 'wifi' : 'usb')
    setEndpoint(device.serial)
    setDisplayName((device.model || device.product || device.serial).replaceAll('_', ' '))
  }

  return <Dialog open={open} onOpenChange={next => { if (!pending) onOpenChange(next) }}>
    <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto" showCloseButton={!pending}>
      <DialogHeader><DialogTitle>添加设备</DialogTitle><DialogDescription>选择设备或填写连接信息，添加后即可预览。</DialogDescription></DialogHeader>
      <div className="flex flex-col gap-3">
        <Button type="button" variant="outline" disabled={Boolean(pending) || Boolean(registered)} onClick={() => void readDiscovery()}>
          <RefreshCw data-icon="inline-start" />{pending === 'discovery' ? '正在读取…' : '读取已发现设备'}
        </Button>
        <WirelessDiscovery key={wirelessGeneration} disabled={Boolean(pending) || Boolean(registered)} onBusyChange={active => setPending(active ? 'wireless' : '')} onConnected={(found, address) => {
          setDiscovery(found)
          const connected = found.find(item => item.serial === address)
          if (connected) selectDiscovered(connected)
        }} />
        <Button type="button" disabled={Boolean(pending) || Boolean(registered)} onClick={() => void registerAll()}>{pending === 'all' ? '正在添加全部设备…' : '一键添加所有设备'}</Button>
        {discovery !== null && <div className="flex flex-col gap-3">
          {discovery.length ? discovery.map(found => <div key={found.serial} className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0"><p className="break-all text-sm">{found.serial}</p><p className="text-xs text-muted-foreground">{found.model} · {found.state}</p></div>
            <Button type="button" variant="outline" size="sm" disabled={Boolean(pending) || Boolean(registered)} onClick={() => selectDiscovered(found)}>使用此设备</Button>
          </div>) : <p className="text-sm text-muted-foreground">当前没有待添加设备。</p>}
        </div>}
      </div>
      <form onSubmit={register}><FieldGroup>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel htmlFor="new-device-name">设备名称</FieldLabel><Input id="new-device-name" value={displayName} onChange={event => setDisplayName(event.target.value)} maxLength={120} required disabled={Boolean(pending) || Boolean(registered)} /></Field>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel>连接方式</FieldLabel><ToggleGroup variant="outline" value={[kind]} disabled={Boolean(pending) || Boolean(registered)} onValueChange={value => { if (value[0]) setKind(value[0] as LinkKind) }} aria-label="连接方式"><ToggleGroupItem value="usb"><Cable data-icon="inline-start" />USB</ToggleGroupItem><ToggleGroupItem value="wifi"><Wifi data-icon="inline-start" />Wi-Fi</ToggleGroupItem></ToggleGroup></Field>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel htmlFor="new-device-endpoint">{kind === 'usb' ? 'USB 序列号' : 'Wi-Fi 连接地址'}</FieldLabel><Input id="new-device-endpoint" value={endpoint} onChange={event => setEndpoint(event.target.value)} placeholder={kind === 'usb' ? '从已发现设备中选择或输入序列号' : '主机:连接端口'} required disabled={Boolean(pending) || Boolean(registered)} /></Field>
        {kind === 'wifi' && <>
          <Field data-disabled={Boolean(pending) || Boolean(registered && registered.stage !== 'pair')}><FieldLabel htmlFor="new-device-pairing-endpoint">配对地址（可选）</FieldLabel><Input id="new-device-pairing-endpoint" value={pairingEndpoint} onChange={event => setPairingEndpoint(event.target.value)} placeholder="主机:配对端口" autoComplete="off" disabled={Boolean(pending) || Boolean(registered && registered.stage !== 'pair')} /><FieldDescription>已配对可留空，配对端口与连接端口不同。</FieldDescription></Field>
          <Field data-disabled={Boolean(pending) || Boolean(registered && registered.stage !== 'pair')}><FieldLabel htmlFor="new-device-pairing-code">临时配对码</FieldLabel><Input id="new-device-pairing-code" type="password" value={pairingCode} onChange={event => setPairingCode(event.target.value)} autoComplete="off" disabled={Boolean(pending) || Boolean(registered && registered.stage !== 'pair')} /></Field>
        </>}
        <DialogFooter><Button type="button" variant="outline" disabled={Boolean(pending)} onClick={() => onOpenChange(false)}>{registered ? '关闭' : '取消'}</Button><Button type="submit" disabled={Boolean(pending) || !displayName.trim() || !endpoint.trim()}>{pending === 'register' ? '正在添加…' : pending === 'pair' ? '正在配对…' : pending === 'verify' ? '正在核验…' : pending === 'refresh' ? '正在刷新…' : registered ? '继续添加' : '添加设备'}</Button></DialogFooter>
      </FieldGroup></form>
    </DialogContent>
  </Dialog>
}

