import { useState } from 'react'
import { KeyRound, LayoutGrid, LogOut, Monitor, MonitorSmartphone, Moon, RefreshCw, Smartphone, Sun } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toast } from '@/components/ui/toast'
import { useAuth } from '@/lib/auth'
import { api, errorMessage } from '@/lib/api'
import { useDevices, usePlatformEvents } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { Device, SessionMode } from '@/lib/types'
import { AdminLogin } from './AdminLogin'
import { CallersPanel } from './CallersPanel'
import { DeviceAdministration } from './DeviceAdministration'
import { DevicePool } from './DevicePool'
import { Workbench } from '@/workbench/Workbench'
import { useTheme } from '@/lib/theme'
import { ClipboardHistoryProvider } from '@/workbench/ClipboardHistory'

type Page = 'devices' | 'workbench' | 'callers'

export function ManagementApp() {
  const { loading, session } = useAuth()
  if (loading) return <main className="grid min-h-svh place-items-center px-4 text-sm text-muted-foreground">正在检查管理员会话…</main>
  return session ? <ClipboardHistoryProvider><AuthenticatedManagement /></ClipboardHistoryProvider> : <AdminLogin />
}

function AuthenticatedManagement() {
  const { logout } = useAuth()
  const theme = useTheme()
  const queryClient = useQueryClient()
  const devices = useDevices()
  const events = usePlatformEvents()
  const [page, setPage] = useState<Page>('devices')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [mode, setMode] = useState<SessionMode>('control')
  const [managedDeviceId, setManagedDeviceId] = useState<string | null>(null)
  const [deviceDialogOpen, setDeviceDialogOpen] = useState(false)
  const [addDeviceOpen, setAddDeviceOpen] = useState(false)
  const [syncingDeviceId, setSyncingDeviceId] = useState<string | null>(null)
  const selectedDevice = devices.data?.find(device => device.deviceId === selectedId) ?? null
  const managedDevice = devices.data?.find(device => device.deviceId === managedDeviceId) ?? null
  useErrorNotification('状态通知异常', events.error)
  useErrorNotification('无法读取设备', devices.error)

  async function handleLogout() {
    try {
      await logout()
    } catch (error) {
      toast.add({ title: '退出登录未确认', description: errorMessage(error), type: 'error', timeout: 0 })
    }
  }

  async function syncDevice(device: Device) {
    if (syncingDeviceId) return
    setSyncingDeviceId(device.deviceId)
    let failure = ''
    try {
      await api<void>('/devices/' + encodeURIComponent(device.deviceId) + '/refresh', { method: 'POST' })
    } catch (error) {
      failure = errorMessage(error)
    }
    try {
      await devices.refetch({ throwOnError: true })
    } catch (error) {
      failure += `${failure ? ' ' : ''}设备列表读取失败：${errorMessage(error)}`
    }
    if (failure) toast.add({ title: '设备刷新未完成', description: failure, type: 'error', timeout: 0 })
    else toast.add({ title: '设备状态已刷新', type: 'success' })
    setSyncingDeviceId(null)
  }

  function openWorkbench(device: Device) {
    setSelectedId(device.deviceId)
    setPage('workbench')
  }

  function changeWorkbenchDevice(device: Device) {
    setSelectedId(device.deviceId)
  }

  return (
    <div className="app-shell">
      <nav className="app-rail" aria-label="主导航">
        <div className="app-brand" title="DroidHarbor" aria-label="DroidHarbor"><MonitorSmartphone /></div>
        <ToggleGroup orientation="vertical" value={[page]} onValueChange={values => { if (values[0]) setPage(values[0] as Page) }} className="rail-group" aria-label="页面">
          <ToggleGroupItem value="devices" aria-label="设备池" title="设备池"><LayoutGrid /><span>设备池</span></ToggleGroupItem>
          <ToggleGroupItem value="workbench" aria-label="工作台" title="工作台"><Smartphone /><span>工作台</span></ToggleGroupItem>
          <ToggleGroupItem value="callers" aria-label="调用凭证" title="调用凭证"><KeyRound /><span>凭证</span></ToggleGroupItem>
        </ToggleGroup>
        <div className="mt-auto flex flex-col items-center gap-3">
          <ToggleGroup orientation="vertical" value={[theme.mode]} onValueChange={values => { const next = values[0]; if (next === 'light' || next === 'dark' || next === 'system') theme.setMode(next) }} className="theme-group" aria-label="主题">
            <ToggleGroupItem value="light" aria-label="浅色模式" title="浅色模式"><Sun /></ToggleGroupItem>
            <ToggleGroupItem value="dark" aria-label="深色模式" title="深色模式"><Moon /></ToggleGroupItem>
            <ToggleGroupItem value="system" aria-label="跟随系统" title="跟随系统"><Monitor /></ToggleGroupItem>
          </ToggleGroup>
          <Button variant="ghost" size="icon" aria-label="退出登录" title="退出登录" onClick={() => void handleLogout()}><LogOut /></Button>
        </div>
      </nav>
      <main className={page === 'workbench' ? 'app-main workbench-page' : 'app-main management-page'}>
        {!devices.discoveryReady && !devices.isPending && <Alert variant="destructive" className="shrink-0"><AlertTitle>设备发现源不可用</AlertTitle><AlertDescription>设备连接状态暂不可用。</AlertDescription></Alert>}
        {devices.isError && <Button className="self-start" variant="outline" size="sm" onClick={() => void devices.refetch()}><RefreshCw data-icon="inline-start" />重新读取设备</Button>}
        {page === 'devices' && (devices.isPending ? <Card><CardHeader><CardTitle>正在读取设备池</CardTitle><CardDescription>设备加载中。</CardDescription></CardHeader></Card> : devices.data && <DevicePool devices={devices.data} onOpen={openWorkbench} onManage={device => { setManagedDeviceId(device.deviceId); setDeviceDialogOpen(true) }} onAdd={() => setAddDeviceOpen(true)} onSync={device => void syncDevice(device)} syncingDeviceId={syncingDeviceId} />)}
        {page === 'workbench' && (selectedDevice ? <Workbench device={selectedDevice} devices={devices.data ?? []} onDeviceChange={changeWorkbenchDevice} mode={mode} onModeChange={setMode} /> : <Empty className="m-auto border"><EmptyHeader><EmptyMedia variant="icon"><Smartphone /></EmptyMedia><EmptyTitle>尚未选择设备</EmptyTitle><EmptyDescription>从设备池选择一台设备后进入工作台。</EmptyDescription></EmptyHeader><Button variant="outline" onClick={() => setPage('devices')}>返回设备池</Button></Empty>)}
        {page === 'callers' && <CallersPanel />}
      </main>
      <DeviceAdministration open={deviceDialogOpen} onOpenChange={setDeviceDialogOpen} device={managedDevice} onChanged={async () => { await queryClient.invalidateQueries({ queryKey: ['devices'] }) }} addDeviceOpen={addDeviceOpen} onAddDeviceOpenChange={setAddDeviceOpen} />
    </div>
  )
}
