import { useEffect, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Ellipsis, FolderPlus, LoaderCircle, Pencil, RotateCcw, Trash2, Upload } from 'lucide-react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from '@/components/ui/dropdown-menu'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { api, errorMessage } from '@/lib/api'

type Request =
  | { action: 'mkdir' | 'rename'; path: string; name: string }
  | { action: 'trash'; path: string }
  | { action: 'delete'; path: string; confirmed: true }
  | { action: 'restore'; id: string }
  | { action: 'purge'; id: string; confirmed: true }
type TrashEntry = { id: string; originalPath: string; kind: 'directory' | 'file'; deletedAt: number }
type RemovalTarget =
  | { action: 'trash' | 'delete'; paths: string[]; batch?: boolean }
  | { action: 'purge'; id: string; path: string }
type RemovalState = { target: RemovalTarget } & (
  | { phase: 'first' | 'confirm' }
  | { phase: 'running'; completed: number }
  | { phase: 'failed'; completed: number; error: string }
)
export type FileMenuAction = 'mkdir' | 'rename' | 'trash' | 'delete' | 'refresh' | 'upload'
const trashTime = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short' })

export function useDeviceFileActions(sessionId: string, onRemoved: (paths: string[]) => void) {
  const [busy, setRunning] = useState(false)
  const controller = useRef<AbortController | null>(null)
  const [error, setError] = useState('')
  const [removal, setRemoval] = useState<RemovalState>()
  const queries = useQueryClient()
  const files = '/sessions/' + encodeURIComponent(sessionId) + '/files'
  useEffect(() => () => controller.current?.abort(), [])
  const runBatch = async (requests: Request[], onProgress?: (completed: number) => void) => {
    if (controller.current) return
    const started = new AbortController()
    controller.current = started
    setError('')
    setRunning(true)
    let completed = 0
    let problem: string | undefined
    try {
      for (const request of requests) {
        await api<void>(files + '/actions', { method: 'POST', body: request, signal: started.signal })
        completed++
        onProgress?.(completed)
      }
    } catch (cause) {
      problem = errorMessage(cause)
    } finally {
      controller.current = null
      if (!started.signal.aborted) {
        setRunning(false)
        void queries.invalidateQueries({ queryKey: ['device-directory', sessionId] })
        void queries.invalidateQueries({ queryKey: ['device-trash', sessionId] })
      }
    }
    if (!started.signal.aborted) return { completed, error: problem }
  }
  const run = async (request: Request) => {
    const result = await runBatch([request])
    if (result?.error) setError(result.error)
    return result?.completed === 1
  }
  const executeRemoval = async (target: RemovalTarget) => {
    if (controller.current) return
    const requests: Request[] = target.action === 'purge' ? [{ action: 'purge', id: target.id, confirmed: true }]
      : target.paths.map(path => target.action === 'trash' ? { action: 'trash', path } : { action: 'delete', path, confirmed: true })
    setRemoval({ target, phase: 'running', completed: 0 })
    const result = await runBatch(requests, completed => setRemoval({ target, phase: 'running', completed }))
    if (!result) return
    if (target.action !== 'purge' && result.completed > 0) onRemoved(target.paths.slice(0, result.completed))
    setRemoval(result.error ? { target, phase: 'failed', completed: result.completed, error: result.error } : undefined)
  }
  const requestRemoval = (target: RemovalTarget) => {
    if (controller.current) return
    if (target.action === 'trash') void executeRemoval(target)
    else setRemoval({ target, phase: target.action === 'delete' && target.batch ? 'confirm' : 'first' })
  }
  const confirmRemoval = () => {
    if (removal?.phase === 'first') setRemoval({ target: removal.target, phase: 'confirm' })
    else if (removal?.phase === 'confirm') void executeRemoval(removal.target)
  }
  const closeRemoval = () => {
    if (!controller.current) setRemoval(undefined)
  }
  return { run, busy, error, removal, requestRemoval, confirmRemoval, closeRemoval }
}

type Operation = ReturnType<typeof useDeviceFileActions>

export function FileActionMenu({ path, name, mutable, writable, disabled, onAction }: {
  path: string; name: string; mutable: boolean; writable: boolean; disabled: boolean
  onAction: (action: FileMenuAction, path: string) => void
}) {
  return <DropdownMenu>
    <DropdownMenuTrigger aria-label={`${name}的操作`} disabled={disabled}
      render={<Button variant="ghost" size="icon-sm" className="shrink-0" />}><Ellipsis /></DropdownMenuTrigger>
    <DropdownMenuContent align="end">
      {writable && <>
        <DropdownMenuItem onClick={() => onAction('mkdir', path)}><FolderPlus />新建目录</DropdownMenuItem>
        <DropdownMenuItem onClick={() => onAction('upload', path)}><Upload />上传到此目录</DropdownMenuItem>
      </>}
      {mutable && <>
        <DropdownMenuItem onClick={() => onAction('rename', path)}><Pencil />重命名</DropdownMenuItem>
        <DropdownMenuItem onClick={() => onAction('trash', path)}><Trash2 />移入回收站</DropdownMenuItem>
        <DropdownMenuItem variant="destructive" onClick={() => onAction('delete', path)}><Trash2 />永久删除</DropdownMenuItem>
      </>}
      <DropdownMenuItem onClick={() => onAction('refresh', path)}><RotateCcw />刷新列表</DropdownMenuItem>
    </DropdownMenuContent>
  </DropdownMenu>
}

export type FileActionDraft = { action: 'mkdir' | 'rename'; path: string }

export function FileActionPrompt({ pending, setPending, disabled, operation, changed }: {
  pending: FileActionDraft; setPending: (value?: FileActionDraft) => void; disabled: boolean
  operation: Operation; changed: (paths: string[]) => void
}) {
  const [name, setName] = useState(() => pending.action === 'rename' ? pending.path.slice(pending.path.lastIndexOf('/') + 1) : '')
  const submit = async () => {
    if (disabled || operation.busy) return
    if (await operation.run({ action: pending.action, path: pending.path, name })) {
      if (pending.action === 'rename') changed([pending.path])
      setPending(undefined)
    }
  }
  return <form onSubmit={event => { event.preventDefault(); void submit() }}>
    <FieldGroup className="gap-1">
      <Field>
        <FieldLabel htmlFor="device-entry-name">{pending.action === 'mkdir' ? '新目录名称' : '新名称'}</FieldLabel>
        <Input id="device-entry-name" value={name} onChange={event => setName(event.currentTarget.value)} disabled={operation.busy || disabled} />
      </Field>
      <div className="flex justify-end gap-1">
        <Button size="sm" variant="ghost" type="button" disabled={operation.busy} onClick={() => setPending(undefined)}>取消</Button>
        <Button size="sm" type="submit" disabled={!name.trim() || operation.busy || disabled}>确认</Button>
      </div>
    </FieldGroup>
  </form>
}

export function FileRemovalDialog({ operation, disabled }: { operation: Operation; disabled: boolean }) {
  const state = operation.removal
  if (!state) return null
  const { target, phase } = state
  const trash = target.action === 'trash'
  const batch = target.action !== 'purge' && target.batch
  const total = target.action === 'purge' ? 1 : target.paths.length
  const path = target.action === 'purge' ? target.path : batch ? undefined : target.paths[0]
  const title = phase === 'running' ? (trash ? '正在移入回收站' : '正在删除')
    : phase === 'failed' ? (trash ? '移入回收站未全部完成' : '未能确认全部删除')
    : batch ? `永久删除 ${total} 项？` : phase === 'first' ? '永久删除' : '再次确认永久删除'
  return <Dialog open disablePointerDismissal={phase === 'running'} onOpenChange={open => { if (!open) operation.closeRemoval() }}>
    <DialogContent showCloseButton={false}>
      <DialogHeader>
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription className="wrap-anywhere">
          {phase === 'running' || phase === 'failed' ? (
            path ?? `共 ${total} 项`
          ) : (
            <>{path && <span className="mb-1 block">{path}</span>}目录内的所有内容也会删除。此操作不经过回收站，无法撤销。</>
          )}
        </DialogDescription>
      </DialogHeader>
      {phase === 'running' && <p role="status" className="flex items-center gap-2 tabular-nums">
        <LoaderCircle className="size-5 motion-safe:animate-spin" aria-hidden="true" />已完成 {state.completed} / {total} 项
      </p>}
      {phase === 'failed' && <Alert variant="destructive">
        <AlertDescription className="wrap-anywhere">已完成 {state.completed} / {total} 项。{state.error}</AlertDescription>
      </Alert>}
      {phase !== 'running' && <DialogFooter>
        <DialogClose render={<Button variant="outline" />}>{phase === 'failed' ? '关闭' : '取消'}</DialogClose>
        {phase !== 'failed' && <Button variant="destructive" disabled={disabled || operation.busy} onClick={operation.confirmRemoval}>
          {phase === 'first' ? '继续' : '确认永久删除'}
        </Button>}
      </DialogFooter>}
    </DialogContent>
  </Dialog>
}

export function DeviceTrash({ sessionId, active, operation }: { sessionId: string; active: boolean; operation: Operation }) {
  const list = useQuery({
    queryKey: ['device-trash', sessionId],
    queryFn: ({ signal }) => api<TrashEntry[]>('/sessions/' + encodeURIComponent(sessionId) + '/files/trash', { signal }),
    enabled: active,
    retry: false,
    refetchOnWindowFocus: false,
  })
  const restore = (id: string) => void operation.run({ action: 'restore', id })
  return <section aria-label="手机回收站" className="flex min-h-0 flex-1 flex-col gap-2 overflow-auto p-2">
    <header className="flex items-center justify-between gap-2">
      <h3 className="font-medium">手机回收站</h3>
      <Button size="icon-sm" variant="ghost" disabled={!active || list.isFetching || operation.busy} aria-label="刷新回收站" onClick={() => void list.refetch()}><RotateCcw /></Button>
    </header>
    {list.isPending && <p role="status" className="flex items-center gap-2"><LoaderCircle className="size-4 motion-safe:animate-spin" />正在读取回收站…</p>}
    {list.error && <Alert variant="destructive"><AlertDescription>{errorMessage(list.error)}</AlertDescription></Alert>}
    {list.data?.length === 0 && <p className="text-muted-foreground">回收站为空</p>}
    {list.data && <ul aria-label="回收站条目">
      {list.data.map(item => <li key={item.id} className="flex min-w-0 flex-col gap-1 border-b py-2">
        <p className="min-w-0 truncate" title={item.originalPath}>{item.originalPath.slice(item.originalPath.lastIndexOf('/') + 1)}</p>
        <p className="break-all text-xs text-muted-foreground">{item.originalPath}</p>
        <p className="text-xs text-muted-foreground">{item.kind === 'directory' ? '目录' : '文件'} · {trashTime.format(item.deletedAt * 1000)}</p>
        <div className="flex flex-wrap gap-1">
          <Button size="sm" variant="outline" disabled={!active || operation.busy} onClick={() => restore(item.id)}>还原</Button>
          <Button size="sm" variant="ghost" disabled={!active || operation.busy} onClick={() => operation.requestRemoval({ action: 'purge', id: item.id, path: item.originalPath })}>永久删除</Button>
        </div>
      </li>)}
    </ul>}
  </section>
}
