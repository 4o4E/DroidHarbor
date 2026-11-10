import { useEffect, useState, type FormEvent } from 'react'
import { Cable, RefreshCw, ShieldAlert, Wifi } from 'lucide-react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Separator } from '@/components/ui/separator'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toast } from '@/components/ui/toast'
import { api, ApiError, errorMessage } from '@/lib/api'
import type { Device, DeviceLink } from '@/lib/types'

type DiscoveredDevice = { serial: string; state: string; transportId: number; model?: string; product?: string }
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
  const [endpoints, setEndpoints] = useState<Record<string, string>>({})
  const [pairing, setPairing] = useState<Record<string, { endpoint: string; code: string }>>({})
  const [newLinkKind, setNewLinkKind] = useState<LinkKind>('wifi')
  const [newLinkEndpoint, setNewLinkEndpoint] = useState('')
  const [busy, setBusy] = useState('')
  const [maintenanceConfirmation, setMaintenanceConfirmation] = useState(false)

  useEffect(() => {
    if (!open || !device) return
    setDisplayName(device.displayName)
    setTagsText(device.tags.join(', '))
    setEndpoints(Object.fromEntries(device.links.map(link => [link.linkId, link.endpoint])))
    setPairing({})
    setNewLinkKind('wifi')
    setNewLinkEndpoint('')
    setMaintenanceConfirmation(false)
  }, [open, device?.deviceId, device?.version])

  async function perform<T>(action: string, request: () => Promise<T>, success: string | ((result: T) => string)) {
    if (busy) return
    setBusy(action)
    try {
      const result = await request()
      const message = typeof success === 'function' ? success(result) : success
      await onChanged()
      toast.add({ title: '操作完成', description: message, type: 'success' })
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

  async function requestMaintenance() {
    if (!device) return
    const next = !device.maintenanceRequested
    await perform('maintenance', () => api<void>('/devices/' + encodeURIComponent(device.deviceId), {
      method: 'PATCH', body: { version: device.version, maintenanceRequested: next },
    }), next ? '维护请求已提交；请读取最新状态确认回收结果。' : '恢复分配请求已提交；请读取最新状态确认设备状态。')
    setMaintenanceConfirmation(false)
  }

  async function recoverDevice() {
    if (!device) return
    await perform('recover', () => api<void>('/devices/' + encodeURIComponent(device.deviceId) + '/recover', { method: 'POST' }), '重新核验请求已提交；离线设备不保证可分配，请确认最新状态。')
  }

  async function addLink(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!device || busy || !newLinkEndpoint.trim()) return
    await perform('add-link', () => api<{ deviceId: string; linkId: string }>('/devices/' + encodeURIComponent(device.deviceId) + '/links', {
      method: 'POST', body: { kind: newLinkKind, endpoint: newLinkEndpoint.trim() },
    }), '新链路已登记，仍需按需连接并核验身份。')
    setNewLinkEndpoint('')
  }

  async function updateEndpoint(link: DeviceLink) {
    if (!device || busy) return
    const endpoint = endpoints[link.linkId]?.trim() ?? ''
    if (!endpoint || endpoint === link.endpoint) return
    await perform('endpoint-' + link.linkId, () => api<void>('/devices/' + encodeURIComponent(device.deviceId) + '/links/' + encodeURIComponent(link.linkId), {
      method: 'PATCH', body: { version: device.version, endpoint },
    }), '地址已更新；新地址仍需显式连接并核验。')
  }

  async function pairLink(link: DeviceLink) {
    if (!device || busy) return
    const values = pairing[link.linkId] ?? { endpoint: '', code: '' }
    const pairingEndpoint = values.endpoint.trim()
    const pairingCode = values.code.trim()
    if (!pairingEndpoint || !pairingCode) {
      toast.add({ title: '请填写手机显示的临时配对地址与配对码', type: 'warning' })
      return
    }
    setPairing(current => ({ ...current, [link.linkId]: { endpoint: '', code: '' } }))
    await perform('pair-' + link.linkId, () => api<void>('/devices/' + encodeURIComponent(device.deviceId) + '/links/' + encodeURIComponent(link.linkId) + '/pair', {
      method: 'POST', body: { pairingEndpoint, pairingCode },
    }), '配对请求已确认。配对本身不代表链路已连接或身份已核验。')
  }

  async function connectLink(link: DeviceLink) {
    if (!device || busy || link.kind !== 'wifi') return
    await perform('connect-' + link.linkId, () => api<void>('/devices/' + encodeURIComponent(device.deviceId) + '/links/' + encodeURIComponent(link.linkId) + '/connect', { method: 'POST' }), '连接请求已确认；请检查最新连接状态。')
  }

  async function verifyLink(link: DeviceLink) {
    if (!device || busy) return
    await perform('verify-' + link.linkId, () => api<{ serial: string; manufacturer: string; model: string; product: string; android: string; sdk: string }>('/devices/' + encodeURIComponent(device.deviceId) + '/links/' + encodeURIComponent(link.linkId) + '/verify', { method: 'POST' }), evidence => {
      const identity = [evidence.manufacturer, evidence.model, evidence.product].filter(Boolean).join(' / ')
      return `身份核验成功：${identity} · ${evidence.serial} · Android ${evidence.android} (SDK ${evidence.sdk})。`
    })
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
    <Dialog open={open && !!device} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-3xl overflow-y-auto">
        <DialogHeader><DialogTitle>管理设备</DialogTitle><DialogDescription>{device?.displayName ?? ''}</DialogDescription></DialogHeader>
        {device && <div className="flex flex-col gap-5">

          <Card size="sm">
            <CardHeader><CardTitle>设备信息</CardTitle></CardHeader>
            <CardContent>
              <form onSubmit={saveDetails}><FieldGroup>
                <Field><FieldLabel htmlFor="managed-device-name">设备名称</FieldLabel><Input id="managed-device-name" value={displayName} onChange={event => setDisplayName(event.target.value)} maxLength={120} required /></Field>
                <Field><FieldLabel htmlFor="managed-device-tags">标签</FieldLabel><Input id="managed-device-tags" value={tagsText} onChange={event => setTagsText(event.target.value)} placeholder="以逗号分隔" /><FieldDescription>标签用于精确筛选；不与维护状态变更捆绑。</FieldDescription></Field>
                <Button type="submit" disabled={Boolean(busy)}>{busy === 'details' ? '正在保存…' : '保存名称与标签'}</Button>
              </FieldGroup></form>
              <Separator className="my-5" />
              <div className="flex flex-wrap items-center justify-between gap-3"><div><p className="text-sm font-medium">分配维护</p><p className="text-xs text-muted-foreground">当前意图：{device.maintenanceRequested ? '暂停分配' : '允许按其他状态分配'}</p></div><Button variant={device.maintenanceRequested ? 'outline' : 'destructive'} disabled={Boolean(busy)} onClick={() => setMaintenanceConfirmation(current => !current)}>{device.maintenanceRequested ? '请求恢复分配' : '停止分配并进入维护'}</Button></div>
              {maintenanceConfirmation && <Alert className="mt-4"><ShieldAlert /><AlertTitle>{device.maintenanceRequested ? '确认请求恢复分配？' : '确认停止分配？'}</AlertTitle><AlertDescription>此维护请求独立提交，可能结束现有租约并进入回收流程；需重新读取设备状态确认完成。</AlertDescription><div className="mt-3 flex gap-2"><Button size="sm" disabled={Boolean(busy)} onClick={() => void requestMaintenance()}>{busy === 'maintenance' ? '正在提交…' : '确认提交'}</Button><Button size="sm" variant="outline" onClick={() => setMaintenanceConfirmation(false)}>取消</Button></div></Alert>}
              {(device.allocationState === 'quarantined' || device.allocationState === 'reclaiming') && <Button className="mt-4" variant="outline" disabled={Boolean(busy)} onClick={() => void recoverDevice()}>{busy === 'recover' ? '正在重新核验…' : '重新核验并回收'}</Button>}
            </CardContent>
          </Card>

          <Card size="sm">
            <CardHeader><CardTitle>已登记链路</CardTitle></CardHeader>
            <CardContent className="flex flex-col gap-5">
              {device.links.length ? device.links.map((link, index) => <div key={link.linkId} className="flex flex-col gap-4">
                {index > 0 && <Separator />}
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div><div className="flex items-center gap-2">{link.kind === 'usb' ? <Cable className="size-4" /> : <Wifi className="size-4" />}<span className="font-medium">{link.kind === 'usb' ? 'USB 链路' : 'Wi-Fi 链路'}</span><Badge variant={link.connectionState === 'online' ? 'secondary' : link.connectionState === 'unauthorized' ? 'destructive' : 'outline'}>{link.connectionState === 'online' ? '在线' : link.connectionState === 'unauthorized' ? '未授权' : '离线'}</Badge><Badge variant={link.identityVerified ? 'secondary' : 'outline'}>{link.identityVerified ? '身份已绑定' : '待核验'}</Badge></div><p className="mt-1 break-all text-xs text-muted-foreground">{link.endpoint}</p>{link.lastError && <p className="mt-1 text-xs text-destructive">{link.lastError}</p>}</div>
                  <div className="flex flex-wrap gap-2"><Button size="sm" variant="outline" disabled={Boolean(busy)} onClick={() => void verifyLink(link)}>{busy === 'verify-' + link.linkId ? '核验中…' : '核验身份'}</Button>{link.kind === 'wifi' && <Button size="sm" variant="outline" disabled={Boolean(busy)} onClick={() => void connectLink(link)}>{busy === 'connect-' + link.linkId ? '连接中…' : '连接'}</Button>}</div>
                </div>
                {link.kind === 'wifi' && <FieldGroup>
                  <Field><FieldLabel htmlFor={`pair-endpoint-${link.linkId}`}>手机显示的配对地址</FieldLabel><Input id={`pair-endpoint-${link.linkId}`} value={pairing[link.linkId]?.endpoint ?? ''} onChange={event => setPairing(current => ({ ...current, [link.linkId]: { endpoint: event.target.value, code: current[link.linkId]?.code ?? '' } }))} placeholder="主机:配对端口" autoComplete="off" /></Field>
                  <Field><FieldLabel htmlFor={`pair-code-${link.linkId}`}>临时配对码</FieldLabel><Input id={`pair-code-${link.linkId}`} type="password" value={pairing[link.linkId]?.code ?? ''} onChange={event => setPairing(current => ({ ...current, [link.linkId]: { endpoint: current[link.linkId]?.endpoint ?? '', code: event.target.value } }))} autoComplete="off" /></Field>
                  <Button className="self-start" size="sm" variant="outline" disabled={Boolean(busy)} onClick={() => void pairLink(link)}>{busy === 'pair-' + link.linkId ? '正在配对…' : '配对'}</Button>
                  <Field><FieldLabel htmlFor={`new-endpoint-${link.linkId}`}>更新连接地址</FieldLabel><Input id={`new-endpoint-${link.linkId}`} value={endpoints[link.linkId] ?? ''} onChange={event => setEndpoints(current => ({ ...current, [link.linkId]: event.target.value }))} autoComplete="off" /><FieldDescription>更新后旧链路代次失效；新地址仍需重新连接并核验。</FieldDescription></Field>
                  <Button className="self-start" size="sm" variant="outline" disabled={Boolean(busy) || !endpoints[link.linkId]?.trim() || endpoints[link.linkId]?.trim() === link.endpoint} onClick={() => void updateEndpoint(link)}>{busy === 'endpoint-' + link.linkId ? '正在更新…' : '更新地址'}</Button>
                </FieldGroup>}
              </div>) : <p className="text-sm text-muted-foreground">当前没有登记链路。</p>}
              <Separator />
              <form onSubmit={addLink}><FieldGroup>
                <Field><FieldLabel>添加另一条链路</FieldLabel><ToggleGroup variant="outline" value={[newLinkKind]} onValueChange={value => { if (value[0]) setNewLinkKind(value[0] as LinkKind) }} aria-label="新增链路类型"><ToggleGroupItem value="usb"><Cable data-icon="inline-start" />USB</ToggleGroupItem><ToggleGroupItem value="wifi"><Wifi data-icon="inline-start" />Wi-Fi</ToggleGroupItem></ToggleGroup></Field>
                <Field><FieldLabel htmlFor="new-link-endpoint">链路 endpoint</FieldLabel><Input id="new-link-endpoint" value={newLinkEndpoint} onChange={event => setNewLinkEndpoint(event.target.value)} placeholder={newLinkKind === 'usb' ? 'USB serial' : '主机:连接端口'} required /></Field>
                <Button type="submit" className="self-start" variant="outline" disabled={Boolean(busy) || !newLinkEndpoint.trim()}>{busy === 'add-link' ? '正在登记…' : '登记链路'}</Button>
              </FieldGroup></form>
            </CardContent>
          </Card>
        </div>}
        <DialogFooter><Button variant="outline" onClick={() => onOpenChange(false)}>关闭</Button></DialogFooter>
      </DialogContent>
    </Dialog>
  </>
}

function CreateDeviceDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; onCreated: () => Promise<void> }) {
  const [displayName, setDisplayName] = useState('')
  const [kind, setKind] = useState<LinkKind>('usb')
  const [endpoint, setEndpoint] = useState('')
  const [discovery, setDiscovery] = useState<DiscoveredDevice[] | null>(null)
  const [registered, setRegistered] = useState<{ deviceId: string; linkId: string } | null>(null)
  const [pending, setPending] = useState('')

  useEffect(() => {
    if (!open) return
    setDisplayName('')
    setKind('usb')
    setEndpoint('')
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

  async function register(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const name = displayName.trim()
    const address = endpoint.trim()
    if (!name || !address || pending) return
    setPending(registered ? 'verify' : 'register')
    let created = registered
    let failureMessage = ''
    try {
      if (!created) {
        created = await api<{ deviceId: string; linkId: string }>('/devices', { method: 'POST', body: { displayName: name, kind, endpoint: address } })
        setRegistered(created)
      }
      setPending('verify')
      await api('/devices/' + encodeURIComponent(created.deviceId) + '/links/' + encodeURIComponent(created.linkId) + '/verify', { method: 'POST' })
    } catch (error) {
      const unknown = error instanceof ApiError && (error.code === 'RESULT_UNKNOWN' || error.code === 'ADB_RESULT_UNKNOWN')
      failureMessage = created ? `设备已登记，身份核验未完成：${errorMessage(error)} 可重新核验，无需重复登记。` : errorMessage(error)
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
    setKind(/:\d+$/.test(device.serial) ? 'wifi' : 'usb')
    setEndpoint(device.serial)
  }

  return <Dialog open={open} onOpenChange={next => { if (!pending) onOpenChange(next) }}>
    <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto" showCloseButton={!pending}>
      <DialogHeader><DialogTitle>登记设备</DialogTitle><DialogDescription>登记所选设备并核验身份；核验成功后即可投屏。</DialogDescription></DialogHeader>
      <div className="flex flex-col gap-3">
        <Button type="button" variant="outline" disabled={Boolean(pending) || Boolean(registered)} onClick={() => void readDiscovery()}>
          <RefreshCw data-icon="inline-start" />{pending === 'discovery' ? '正在读取…' : '读取已发现设备'}
        </Button>
        {discovery !== null && <div className="flex flex-col gap-3">
          {discovery.length ? discovery.map(found => <div key={found.serial} className="flex flex-wrap items-center justify-between gap-3">
            <div className="min-w-0"><p className="break-all text-sm">{found.serial}</p><p className="text-xs text-muted-foreground">{found.model} · {found.state}</p></div>
            <Button type="button" variant="outline" size="sm" disabled={Boolean(pending) || Boolean(registered)} onClick={() => selectDiscovered(found)}>使用此设备</Button>
          </div>) : <p className="text-sm text-muted-foreground">当前没有已发现设备。</p>}
        </div>}
      </div>
      <form onSubmit={register}><FieldGroup>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel htmlFor="new-device-name">设备名称</FieldLabel><Input id="new-device-name" value={displayName} onChange={event => setDisplayName(event.target.value)} maxLength={120} required disabled={Boolean(pending) || Boolean(registered)} /></Field>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel>第一条链路类型</FieldLabel><ToggleGroup variant="outline" value={[kind]} disabled={Boolean(pending) || Boolean(registered)} onValueChange={value => { if (value[0]) setKind(value[0] as LinkKind) }} aria-label="第一条链路类型"><ToggleGroupItem value="usb"><Cable data-icon="inline-start" />USB</ToggleGroupItem><ToggleGroupItem value="wifi"><Wifi data-icon="inline-start" />Wi-Fi</ToggleGroupItem></ToggleGroup></Field>
        <Field data-disabled={Boolean(pending) || Boolean(registered)}><FieldLabel htmlFor="new-device-endpoint">{kind === 'usb' ? 'USB serial' : 'Wi-Fi endpoint'}</FieldLabel><Input id="new-device-endpoint" value={endpoint} onChange={event => setEndpoint(event.target.value)} placeholder={kind === 'usb' ? '从设备发现列表选择或输入 serial' : '主机:连接端口'} required disabled={Boolean(pending) || Boolean(registered)} /><FieldDescription>配对地址与连接地址分别填写。</FieldDescription></Field>
        <DialogFooter><Button type="button" variant="outline" disabled={Boolean(pending)} onClick={() => onOpenChange(false)}>{registered ? '关闭' : '取消'}</Button><Button type="submit" disabled={Boolean(pending) || !displayName.trim() || !endpoint.trim()}>{pending === 'register' ? '正在登记…' : pending === 'verify' ? '正在核验…' : pending === 'refresh' ? '正在刷新…' : registered ? '重新核验' : '登记并核验'}</Button></DialogFooter>
      </FieldGroup></form>
    </DialogContent>
  </Dialog>
}

