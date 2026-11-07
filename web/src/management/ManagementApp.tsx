import { useState } from 'react'
import { KeyRound, LayoutGrid, LogOut, MonitorSmartphone, RefreshCw, ShieldCheck, Smartphone } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useAuth } from '@/lib/auth'
import { errorMessage } from '@/lib/api'
import { useDevices, usePlatformEvents } from '@/lib/queries'
import type { Device, SessionMode } from '@/lib/types'
import { AdminLogin } from './AdminLogin'
import { CallersPanel } from './CallersPanel'
import { DeviceAdministration } from './DeviceAdministration'
import { DevicePool } from './DevicePool'
import { Workbench } from '@/workbench/Workbench'

type Page = 'devices' | 'workbench' | 'callers'

export function ManagementApp() {
  const { loading, session } = useAuth()
  if (loading) return <main className="grid min-h-svh place-items-center px-4 text-sm text-muted-foreground">正在检查管理员会话…</main>
  return session ? <AuthenticatedManagement /> : <AdminLogin />
}

function AuthenticatedManagement() {
  const { logout } = useAuth()
  const queryClient = useQueryClient()
  const devices = useDevices()
  const events = usePlatformEvents()
  const [page, setPage] = useState<Page>('devices')
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [mode, setMode] = useState<SessionMode>('control')
  const [managedDeviceId, setManagedDeviceId] = useState<string | null>(null)
  const [deviceDialogOpen, setDeviceDialogOpen] = useState(false)
  const [addDeviceOpen, setAddDeviceOpen] = useState(false)
  const [logoutError, setLogoutError] = useState('')
  const selectedDevice = devices.data?.find(device => device.deviceId === selectedId) ?? null
  const managedDevice = devices.data?.find(device => device.deviceId === managedDeviceId) ?? null

  async function handleLogout() {
    setLogoutError('')
    try {
      await logout()
    } catch (error) {
      setLogoutError(errorMessage(error))
    }
  }

  function openWorkbench(device: Device) {
    setSelectedId(device.deviceId)
    setPage('workbench')
  }

  function changeWorkbenchDevice(device: Device) {
    setSelectedId(device.deviceId)
  }

  return (
    <div className="min-h-svh">
      <Tabs value={page} onValueChange={value => setPage(String(value) as Page)} className="gap-0">
        <header className="border-b bg-card">
          <div className="mx-auto flex max-w-[1440px] flex-wrap items-center gap-x-8 gap-y-3 px-4 py-4 sm:px-8">
            <div className="flex items-center gap-2.5"><div className="flex size-9 items-center justify-center rounded-xl bg-primary text-primary-foreground"><MonitorSmartphone className="size-5" /></div><span className="text-lg font-semibold tracking-tight">DroidHarbor</span></div>
            <TabsList variant="line" className="order-3 w-full justify-start sm:order-none sm:w-auto">
              <TabsTrigger value="devices"><LayoutGrid />设备池</TabsTrigger>
              <TabsTrigger value="workbench"><Smartphone />工作台</TabsTrigger>
              <TabsTrigger value="callers"><KeyRound />调用凭证</TabsTrigger>
            </TabsList>
            <div className="ml-auto flex items-center gap-3">
              <Badge variant="outline"><ShieldCheck data-icon="inline-start" />管理员</Badge>
              <Button variant="ghost" size="sm" onClick={() => void handleLogout()}><LogOut data-icon="inline-start" />退出</Button>
            </div>
          </div>
        </header>
        <main className="mx-auto w-full max-w-[1440px] px-4 py-6 sm:px-8 sm:py-8">
          {logoutError && <Alert variant="destructive" className="mb-6"><AlertTitle>退出登录未确认</AlertTitle><AlertDescription>{logoutError}</AlertDescription></Alert>}
          {events.error && <Alert variant="destructive" className="mb-6"><AlertTitle>状态通知异常</AlertTitle><AlertDescription>{events.error}</AlertDescription></Alert>}
          {!devices.discoveryReady && !devices.isPending && <Alert variant="destructive" className="mb-6"><AlertTitle>设备发现源不可用</AlertTitle><AlertDescription>设备连接状态暂不可用。</AlertDescription></Alert>}
          {devices.error && <Alert variant="destructive" className="mb-6"><AlertTitle>无法读取设备</AlertTitle><AlertDescription>{errorMessage(devices.error)}</AlertDescription><Button className="mt-3" variant="outline" size="sm" onClick={() => void devices.refetch()}><RefreshCw data-icon="inline-start" />重新读取</Button></Alert>}
          <TabsContent value="devices" className="flex flex-col gap-6">
            {devices.isPending ? <Card><CardHeader><CardTitle>正在读取设备池</CardTitle><CardDescription>设备加载中。</CardDescription></CardHeader></Card> : devices.data && <DevicePool devices={devices.data} onOpen={openWorkbench} onManage={device => { setManagedDeviceId(device.deviceId); setDeviceDialogOpen(true) }} onAdd={() => setAddDeviceOpen(true)} />}
          </TabsContent>
          <TabsContent value="workbench">
            {selectedDevice ? <Workbench key={selectedDevice.deviceId} device={selectedDevice} devices={devices.data ?? []} onDeviceChange={changeWorkbenchDevice} mode={mode} onModeChange={setMode} onBack={() => setPage('devices')} /> : <Empty className="border"><EmptyHeader><EmptyMedia variant="icon"><Smartphone /></EmptyMedia><EmptyTitle>尚未选择设备</EmptyTitle><EmptyDescription>从设备池选择一台设备后进入工作台。</EmptyDescription></EmptyHeader><Button variant="outline" onClick={() => setPage('devices')}>返回设备池</Button></Empty>}
          </TabsContent>
          <TabsContent value="callers"><CallersPanel /></TabsContent>
        </main>
      </Tabs>
      <DeviceAdministration open={deviceDialogOpen} onOpenChange={setDeviceDialogOpen} device={managedDevice} onChanged={async () => { await queryClient.invalidateQueries({ queryKey: ['devices'] }) }} addDeviceOpen={addDeviceOpen} onAddDeviceOpenChange={setAddDeviceOpen} />
    </div>
  )
}
