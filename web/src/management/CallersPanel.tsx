import { useState, type FormEvent } from 'react'
import { Copy, KeyRound, Plus } from 'lucide-react'
import { useQueryClient } from '@tanstack/react-query'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardFooter, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Separator } from '@/components/ui/separator'
import { toast } from '@/components/ui/toast'
import { api, ApiError, errorMessage } from '@/lib/api'
import { useCallers } from '@/lib/queries'
import { useErrorNotification } from '@/lib/notifications'
import type { Caller } from '@/lib/types'

type CreatedCaller = Caller & { token: string }

export function CallersPanel() {
  const queryClient = useQueryClient()
  const callers = useCallers()
  const [createOpen, setCreateOpen] = useState(false)
  const [revokeCaller, setRevokeCaller] = useState<Caller | null>(null)
  const [created, setCreated] = useState<CreatedCaller | null>(null)
  const [name, setName] = useState('')
  const [pending, setPending] = useState(false)
  const [copied, setCopied] = useState(false)
  useErrorNotification('无法读取调用方', callers.error)

  function closeCreate(open: boolean) {
    if (!open && pending) return
    setCreateOpen(open)
    if (!open) {
      setCreated(null)
      setName('')
      setCopied(false)
    }
  }

  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const normalized = name.trim()
    if (!normalized || pending) return
    setPending(true)
    try {
      const result = await api<CreatedCaller>('/callers', { method: 'POST', body: { name: normalized } })
      setCreated(result)
      await queryClient.invalidateQueries({ queryKey: ['callers'] })
      toast.add({ title: '调用凭证已创建', type: 'success' })
    } catch (error) {
      const unknown = error instanceof ApiError && error.code === 'RESULT_UNKNOWN'
      toast.add({ title: '凭证创建未完成', description: unknown ? `${errorMessage(error)} 刷新列表核对创建状态；新令牌无法恢复。` : errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      setPending(false)
    }
  }

  async function copyToken() {
    if (!created?.token) return
    try {
      await navigator.clipboard.writeText(created.token)
      setCopied(true)
      toast.add({ title: '令牌已复制', type: 'success' })
    } catch (error) {
      toast.add({ title: '凭证尚未安全保存', description: `复制失败：${errorMessage(error)}`, type: 'error', timeout: 0 })
    }
  }

  async function confirmRevoke() {
    if (!revokeCaller || pending) return
    setPending(true)
    try {
      await api<void>('/callers/' + encodeURIComponent(revokeCaller.id), { method: 'DELETE' })
      setRevokeCaller(null)
      await queryClient.invalidateQueries({ queryKey: ['callers'] })
      toast.add({ title: '调用凭证已撤销', type: 'success' })
    } catch (error) {
      const unknown = error instanceof ApiError && error.code === 'RESULT_UNKNOWN'
      toast.add({ title: '撤销状态未确认', description: unknown ? `${errorMessage(error)} 刷新列表核对撤销状态。` : errorMessage(error), type: 'error', timeout: 0 })
    } finally {
      setPending(false)
    }
  }

  return (
    <section className="flex flex-col gap-6">
      <div className="flex items-start justify-between gap-4">
        <div><p className="mb-1 text-xs font-medium uppercase tracking-[0.16em] text-muted-foreground">Access</p><h1 className="text-2xl font-semibold tracking-tight">调用凭证</h1><p className="mt-2 text-sm text-muted-foreground">查看凭证元数据并管理调用权限。</p></div>
        <Button onClick={() => setCreateOpen(true)}><Plus data-icon="inline-start" />创建凭证</Button>
      </div>

      {callers.isError && <Button size="sm" variant="outline" onClick={() => void callers.refetch()}>重新读取调用方</Button>}
      <Card>
        <CardHeader><CardTitle>已授权调用方</CardTitle><CardDescription>明文凭证仅在创建成功时展示一次，列表只提供元数据。</CardDescription></CardHeader>
        <CardContent className="flex flex-col gap-5">
          {callers.isPending ? <p className="text-sm text-muted-foreground">正在读取调用方…</p> : callers.isError && !callers.data ? null : callers.data?.length ? callers.data.map((caller, index) => <div key={caller.id} className="flex flex-col gap-4">
            {index > 0 && <Separator />}
            <div className="flex flex-wrap items-center justify-between gap-4">
              <div className="flex items-center gap-3"><div className="flex size-10 items-center justify-center rounded-lg bg-muted"><KeyRound /></div><div><p className="text-sm font-medium">{caller.name}</p><p className="mt-1 text-xs text-muted-foreground">创建于 {formatDate(caller.createdAt)}</p></div></div>
              <div className="flex items-center gap-4"><Badge variant={caller.revokedAt ? 'outline' : 'secondary'}>{caller.revokedAt ? '已撤销' : '已启用'}</Badge>{!caller.revokedAt && <Button variant="destructive" size="sm" onClick={() => setRevokeCaller(caller)}>撤销</Button>}</div>
            </div>
            {caller.revokedAt && <p className="text-xs text-muted-foreground">撤销于 {formatDate(caller.revokedAt)}</p>}
          </div>) : <Empty><EmptyHeader><EmptyMedia variant="icon"><KeyRound /></EmptyMedia><EmptyTitle>尚无调用凭证</EmptyTitle><EmptyDescription>创建凭证后仅在成功响应中展示一次明文令牌。</EmptyDescription></EmptyHeader></Empty>}
        </CardContent>
        <CardFooter><p className="text-sm text-muted-foreground">撤销会拒绝该调用方及关联任务的后续授权；设备资源回收状态另行确认。</p></CardFooter>
      </Card>

      <Dialog open={createOpen} onOpenChange={closeCreate}>
        <DialogContent>
          <DialogHeader><DialogTitle>{created ? '保存新凭证' : '创建调用凭证'}</DialogTitle><DialogDescription>{created ? '该明文令牌不会再次显示；请立即保存到安全凭证库。' : '为调用方命名。名称仅用于识别，不作为认证身份。'}</DialogDescription></DialogHeader>
          {created ? <FieldGroup>
            <Field><FieldLabel htmlFor="created-caller-token">一次性 bearer 令牌</FieldLabel><Input id="created-caller-token" readOnly value={created.token} autoComplete="off" /><FieldDescription>关闭此窗口后令牌将从页面状态清除。</FieldDescription></Field>
            <p className="text-sm text-muted-foreground">调用方：{created.name} · 创建于 {formatDate(created.createdAt)}</p>
          </FieldGroup> : <form onSubmit={create}>
            <FieldGroup><Field><FieldLabel htmlFor="caller-name">调用方名称</FieldLabel><Input id="caller-name" value={name} onChange={event => setName(event.target.value)} maxLength={120} autoComplete="off" required /><FieldDescription>名称去首尾空白后须为 1–120 个字符。</FieldDescription></Field><Button type="submit" disabled={pending || !name.trim()}>{pending ? '正在创建…' : '创建凭证'}</Button></FieldGroup>
          </form>}
          <DialogFooter>
            {created ? <><Button variant="outline" onClick={() => void copyToken()}><Copy data-icon="inline-start" />{copied ? '已复制' : '复制令牌'}</Button><Button onClick={() => closeCreate(false)}>完成</Button></> : <Button variant="outline" disabled={pending} onClick={() => closeCreate(false)}>取消</Button>}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={revokeCaller !== null} onOpenChange={open => { if (!open && !pending) setRevokeCaller(null) }}>
        <DialogContent>
          <DialogHeader><DialogTitle>确认撤销调用凭证</DialogTitle><DialogDescription>将拒绝“{revokeCaller?.name ?? ''}”及其关联任务的后续授权。撤销不会保证设备已立即回池。</DialogDescription></DialogHeader>
          <DialogFooter><Button variant="outline" disabled={pending} onClick={() => setRevokeCaller(null)}>取消</Button><Button variant="destructive" disabled={pending} onClick={() => void confirmRevoke()}>{pending ? '正在撤销…' : '确认撤销'}</Button></DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}

function formatDate(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}
