import { lazy, memo, Suspense, useCallback, useDeferredValue, useEffect, useLayoutEffect, useMemo, useRef, useState, type MouseEvent, type ReactNode } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronRight, CircleX, Clipboard, File as FileIcon, Folder, FolderOpen, FolderPlus, Link2, LoaderCircle, Package, RefreshCw, Trash2, Upload as UploadIcon, X, type LucideIcon } from 'lucide-react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { ResizableHandle, ResizablePanel, ResizablePanelGroup } from '@/components/ui/resizable'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Toggle } from '@/components/ui/toggle'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { toast } from '@/components/ui/toast'
import { ApiError, api, apiResponse, errorMessage } from '@/lib/api'
import { DeviceTrash, FileActionMenu, FileActionPrompt, FileRemovalDialog, useDeviceFileActions, type FileActionDraft, type FileMenuAction } from './DeviceFileActions'
import type { AppGroup, DeviceApp } from './DeviceAppSort'
import { AppLetterIndex } from './AppLetterIndex'

const sharedPath = '/sdcard'
const batchLimit = 32
const sessionPath = (id: string) => `/sessions/${encodeURIComponent(id)}`
const childPath = (parent: string, name: string) => `${parent === '/' ? '' : parent}/${name}`
const parentPath = (file: string) => file.slice(0, file.lastIndexOf('/')) || '/'
const androidAppParents = new Set(['data', 'media', 'obb'].map(name => `${sharedPath}/Android/${name}`))
const appIconSource = (icon: string) => `data:image/png;base64,${icon}`

type DirectoryEntry = { name: string; kind: 'directory' | 'file' | 'link' | 'other'; size: number; modifiedAt: number }
type Directory = { path: string; rootAvailable: boolean; writable: boolean; entries: DirectoryEntry[] }
type FilePreview = { kind: 'text' | 'image' | 'binary'; size: number; text?: string; mime?: string; image?: string; truncated?: boolean; version?: string; bytes?: string }
type PackageList = { userId: number; apps: DeviceApp[] }
type Removal = { packageName: string; state: 'succeeded' | 'failed' | 'not_started' | 'unknown'; message?: string }
type Transfer = { transferId: string; fileName: string; state: 'uploading' | 'transferring' | 'installing' | 'succeeded' | 'failed' | 'cancelled' | 'unknown'; size: number; bytesReceived: number; bytesTransferred: number; error?: string }
type Upload = { file: File; purpose: 'file' | 'apk'; targetPath?: string; overwrite?: boolean; ifMatch?: string }
type TransferOperation = { busy: boolean; value?: Transfer; problem: string; start: (upload: Upload) => Promise<boolean> }
const transferLabels = { uploading: '正在上传', transferring: '正在写入手机', installing: '正在安装', succeeded: '已完成', failed: '失败', cancelled: '已取消', unknown: '结果未知' } as const
const removalLabels = { succeeded: '已卸载', failed: '失败', not_started: '未执行', unknown: '结果未知' } as const
const terminalTransfer = (state: Transfer['state']) => state !== 'uploading' && state !== 'transferring' && state !== 'installing'

function useDeviceTransfer(sessionId: string): TransferOperation {
  const running = useRef<AbortController | null>(null)
  const [busy, setBusy] = useState(false)
  const [value, setValue] = useState<Transfer>()
  const [problem, setProblem] = useState('')
  useEffect(() => () => running.current?.abort(), [])
  const start = async ({ file, ...metadata }: Upload) => {
    if (running.current) return false
    const controller = new AbortController()
    const { signal } = controller
    running.current = controller
    setBusy(true)
    setProblem('')
    setValue(undefined)
    try {
      const created = await api<Transfer>(`${sessionPath(sessionId)}/transfers`, { method: 'POST', body: { ...metadata, fileName: file.name, size: file.size }, signal })
      const path = `/transfers/${encodeURIComponent(created.transferId)}`
      setValue(created)
      let uploadError: unknown
      const send = apiResponse(`${path}/content`, { method: 'PUT', headers: { 'Content-Type': 'application/octet-stream' }, body: file, signal })
        .then(() => undefined, cause => { uploadError = cause })
      // Observe the server's counters while the browser streams bytes. The
      // upload response alone does not prove that Android finished the work.
      let current = created
      while (!terminalTransfer(current.state)) {
        await new Promise<void>(resolve => window.setTimeout(resolve, 400))
        signal.throwIfAborted()
        if (uploadError) throw uploadError
        current = await api<Transfer>(path, { signal })
        setValue(current)
      }
      await send
      if (current.state !== 'succeeded') {
        setProblem(current.error ?? transferLabels[current.state])
        toast.add({ title: transferLabels[current.state], description: current.error, type: 'error' })
        return false
      }
      return true
    } catch (error) {
      if (!signal.aborted) {
        const message = errorMessage(error)
        setProblem(message)
        toast.add({ title: '传输未确认', description: message, type: 'error', timeout: 0 })
      }
      return false
    } finally {
      if (!signal.aborted) setBusy(false)
      controller.abort()
      if (running.current === controller) running.current = null
    }
  }
  return { busy, value, problem, start }
}

function TransferFeedback({ operation }: { operation: TransferOperation }) {
  const { value, busy, problem } = operation
  if (problem) return <Alert variant="destructive"><AlertDescription>{problem}</AlertDescription></Alert>
  if (!value && !busy) return null
  const bytes = value?.state === 'uploading' ? value.bytesReceived : value?.bytesTransferred
  return <p role="status" className="text-sm text-muted-foreground break-all">
    {value ? `${value.fileName}：${transferLabels[value.state]}` : '正在准备传输'}
    {value && busy && bytes !== undefined && `（${bytes.toLocaleString()} / ${value.size.toLocaleString()} B）`}
  </p>
}

function QueryFeedback({ pending, error, loading = '正在读取设备…' }: { pending: boolean; error: unknown; loading?: string }) {
  if (pending) return <p role="status" className="flex items-center gap-2 px-1 py-2 text-sm text-muted-foreground">
    <LoaderCircle className="size-4 motion-safe:animate-spin" aria-hidden="true" />{loading}
  </p>
  if (!error) return null
  return <Alert variant="destructive"><AlertDescription>{errorMessage(error)}</AlertDescription></Alert>
}

function useDeviceDirectory(sessionId: string, directory: string, enabled: boolean) {
  return useQuery({
    queryKey: ['device-directory', sessionId, directory],
    queryFn: ({ signal }) => api<Directory>(`${sessionPath(sessionId)}/files?path=${encodeURIComponent(directory)}`, { signal }),
    enabled,
    retry: false,
    staleTime: 5000,
    refetchOnWindowFocus: false,
  })
}

function deviceAppsQuery(sessionId: string, scope: 'user' | 'all') {
  return {
    queryKey: ['device-apps', sessionId, scope],
    queryFn: async ({ signal }: { signal: AbortSignal }) => {
      const [value, sorting] = await Promise.all([
        api<PackageList>(`${sessionPath(sessionId)}/apps?scope=${scope}`, { signal }),
        import('./DeviceAppSort'),
      ])
      return { ...value, groups: sorting.groupApps(value.apps) }
    },
    staleTime: 60000,
    refetchOnWindowFocus: false,
    retry: false,
  } as const
}

function appIcons({ apps }: PackageList) {
  const icons = new Map<string, string>()
  for (const app of apps) if (app.icon) icons.set(app.packageName, app.icon)
  return icons
}

const fileDate = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short', hour12: false })
const fileBytes = new Intl.NumberFormat('zh-CN')

function FileTreeIcon({ icon: Icon, appIcon }: { icon: LucideIcon; appIcon?: string }) {
  return <span className="relative grid size-7 shrink-0 place-items-center">
    <Icon className="size-5" />
    {appIcon && <img alt="" src={appIconSource(appIcon)} className="absolute -right-0.5 -bottom-0.5 size-4 rounded-sm bg-background object-contain ring-1 ring-background" />}
  </span>
}

function FileRowLabel({ name, entry }: { name: string; entry?: DirectoryEntry }) {
  return <span className="flex min-w-0 flex-1 flex-col items-start py-1 text-left">
    <span className="max-w-full truncate">{name}</span>
    {entry && <span className="flex flex-wrap gap-x-2 text-[10px] leading-4 font-normal text-muted-foreground tabular-nums">
      {entry.kind === 'file' && <span title="文件大小">{fileBytes.format(entry.size)} B</span>}
      <span className="whitespace-nowrap">修改 {fileDate.format(entry.modifiedAt * 1000)}</span>
    </span>}
  </span>
}

type SelectionModifiers = Pick<MouseEvent, 'shiftKey' | 'ctrlKey' | 'metaKey'>

type TreeProps = {
  sessionId: string
  selected: string
  active: boolean
  busy: boolean
  selection: ReadonlySet<string>
  selectRow: (event: SelectionModifiers, path: string, selectable: boolean, activate: () => void) => void
  collapse: (path: string) => void
  onAction: (action: FileMenuAction, path: string) => void
  select: (directory: string, file?: string) => void
}

function DirectoryBranch({ path, name, entry, root = false, mutable = false, appIcon, ...tree }: TreeProps & {
  path: string; name: string; entry?: DirectoryEntry; root?: boolean; mutable?: boolean; appIcon?: string
}) {
  const [open, setOpen] = useState(root)
  const listing = useDeviceDirectory(tree.sessionId, path, tree.active && open)
  const Icon = open ? FolderOpen : Folder
  const writable = !listing.isError && (listing.data?.writable ?? mutable)
  const Entries = androidAppParents.has(path) ? AndroidAppEntries : DirectoryEntries
  return <li>
    <Collapsible open={open} onOpenChange={value => {
      setOpen(value)
      if (!value && (mutable || root)) tree.collapse(path)
    }}>
      <div className="flex min-w-0 items-center">
        <CollapsibleTrigger
          render={<Button variant={tree.selection.has(path) || !mutable && tree.selected === path ? 'secondary' : 'ghost'}
            size="sm" className="group h-auto min-h-7 min-w-0 flex-1 justify-start px-1" />}
          disabled={tree.busy}
          aria-current={tree.selected === path || undefined} aria-pressed={mutable ? tree.selection.has(path) : undefined}
          aria-label={name} title={path} data-selectable-file={mutable || undefined}
          onClick={event => {
            if (event.shiftKey || event.ctrlKey || event.metaKey) event.preventBaseUIHandler()
            tree.selectRow(event, path, mutable, () => tree.select(path))
          }}
        >
          <ChevronRight className="size-3.5 group-data-panel-open:rotate-90" />
          <FileTreeIcon icon={Icon} appIcon={appIcon} />
          <FileRowLabel name={name} entry={entry} />
        </CollapsibleTrigger>
        <FileActionMenu path={path} name={name} writable={writable} mutable={mutable} disabled={tree.busy} onAction={(action, target) => {
          if (action === 'refresh') {
            if (!open) setOpen(true)
            else void listing.refetch()
          } else tree.onAction(action, target)
        }} />
      </div>
      <CollapsibleContent>
        {open && <>
          <QueryFeedback pending={listing.isPending} error={listing.error} />
          {listing.data && !listing.isError && <Entries path={path} entries={listing.data.entries} tree={tree} writable={writable} refresh={() => void listing.refetch()} />}
        </>}
      </CollapsibleContent>
    </Collapsible>
  </li>
}

type DirectoryEntriesProps = { path: string; entries: DirectoryEntry[]; tree: TreeProps; writable: boolean; refresh: () => void }

function AndroidAppEntries(props: DirectoryEntriesProps) {
  const { data: icons } = useQuery({
    ...deviceAppsQuery(props.tree.sessionId, 'all'),
    enabled: props.tree.active && props.entries.some(entry => entry.kind === 'directory'),
    select: appIcons,
  })
  return <DirectoryEntries {...props} icons={icons} />
}

function DirectoryEntries({ path, entries, tree, writable, refresh, icons }: DirectoryEntriesProps & { icons?: ReadonlyMap<string, string> }) {
  return <ul className="ml-2 border-l">
    {entries.map(entry => {
      const child = childPath(path, entry.name)
      if (entry.kind === 'directory') return <DirectoryBranch key={child} {...tree} path={child} name={entry.name} entry={entry} mutable={writable} appIcon={icons?.get(entry.name)} />
      const regular = entry.kind === 'file'
      const selectable = writable && regular
      const EntryIcon = entry.kind === 'link' ? Link2 : FileIcon
      return <li key={child} className="flex min-w-0 items-center">
        {regular
          ? <Button variant={tree.selection.has(child) || !selectable && tree.selected === child ? 'secondary' : 'ghost'}
            size="sm" className="h-auto min-h-7 min-w-0 flex-1 justify-start pl-[22px]" disabled={tree.busy}
            aria-label={entry.name} title={child} data-selectable-file={selectable || undefined}
            aria-current={tree.selected === child || undefined} aria-pressed={selectable ? tree.selection.has(child) : undefined}
            onClick={event => tree.selectRow(event, child, selectable, () => tree.select(path, child))}>
            <FileTreeIcon icon={EntryIcon} /><FileRowLabel name={entry.name} entry={entry} />
          </Button>
          : <span className="flex min-w-0 flex-1 items-center gap-1 pl-[23px] text-muted-foreground" title={entry.kind === 'link' ? '符号链接' : '特殊文件'}>
            <FileTreeIcon icon={EntryIcon} /><FileRowLabel name={entry.name} entry={entry} />
          </span>}
        <FileActionMenu path={child} name={entry.name} writable={false} mutable={selectable} disabled={tree.busy}
          onAction={(action, target) => action === 'refresh' ? refresh() : tree.onAction(action, target)} />
      </li>
    })}
    {entries.length === 0 && <li className="px-2 py-1 text-muted-foreground">空目录</li>}
  </ul>
}

function batchTargets(selection: ReadonlySet<string>) {
  return Array.from(selection).filter(path => {
    for (let parent = parentPath(path); parent !== '/'; parent = parentPath(parent)) {
      if (selection.has(parent)) return false
    }
    return true
  })
}

const TextEditor = lazy(() => import('./DeviceFileEditors').then(mod => ({ default: mod.TextEditor })))
const BinaryEditor = lazy(() => import('./DeviceBinaryEditor'))
const hexPage = 4096

function DeviceFilePreview({ sessionId, path, active, writable, editing, setEditing, operation, onSaved }: {
  sessionId: string; path: string; active: boolean; writable: boolean; editing: boolean
  setEditing: (value: boolean) => void; operation: TransferOperation; onSaved: () => void
}) {
  const [offset, setOffset] = useState(0)
  const [draft, setDraft] = useState('')
  const [saveProblem, setSaveProblem] = useState('')
  const preview = useQuery({
    queryKey: ['device-file-preview', sessionId, path, offset],
    queryFn: ({ signal }) => api<FilePreview>(`${sessionPath(sessionId)}/files/preview?path=${encodeURIComponent(path)}&offset=${offset}`, { signal }),
    enabled: active && !editing,
    gcTime: 0,
    staleTime: 5000,
    retry: false,
    refetchOnWindowFocus: false,
  })
  const value = preview.data
  const name = path.slice(path.lastIndexOf('/') + 1)
  const begin = () => { setDraft(value?.text ?? ''); setSaveProblem(''); setEditing(true) }
  const save = async () => {
    if (!active || !writable || !value?.version || operation.busy) return
    const edited = new File([draft], name, { type: 'text/plain;charset=utf-8' })
    if (edited.size > 64 * 1024) { setSaveProblem('文本编辑保存上限为 64 KiB'); return }
    setSaveProblem('')
    if (await operation.start({ file: edited, purpose: 'file', targetPath: path, overwrite: true, ifMatch: value.version })) {
      await preview.refetch()
      setEditing(false)
      onSaved()
    }
  }
  return <section className="flex h-full min-h-0 min-w-0 flex-col gap-2 overflow-auto p-2" aria-label="文件预览">
    <div className="flex flex-wrap items-center gap-1">
      <h3 className="min-w-0 flex-1 truncate font-medium" title={path}>{name}</h3>
      {editing
        ? <>
          <Button size="sm" variant="ghost" disabled={operation.busy} onClick={() => { setEditing(false); setSaveProblem('') }}>放弃编辑</Button>
          <Button size="sm" disabled={!active || !writable || operation.busy || (value?.text ?? '') === draft} onClick={() => void save()}>保存</Button>
        </>
        : <>
          {writable && value?.version && <Button variant="outline" size="sm" disabled={!active || preview.isFetching || operation.busy} onClick={begin}>编辑文本</Button>}
          <Button variant="ghost" size="icon-sm" aria-label="刷新文件预览" disabled={!active || preview.isFetching || operation.busy} onClick={() => void preview.refetch()}><RefreshCw /></Button>
        </>}
    </div>
    <QueryFeedback pending={preview.isPending} error={preview.error} />
    {saveProblem && <Alert variant="destructive"><AlertDescription>{saveProblem}</AlertDescription></Alert>}
    {value && (!preview.isError || editing) && <>
      <p className="text-xs text-muted-foreground">{fileBytes.format(value.size)} B{value.truncated && ' · 仅显示文件开头'}{editing ? ' · 编辑中' : ' · 只读'}</p>
      {value.kind === 'text' && <Suspense fallback={<QueryFeedback pending error={null} loading="正在加载编辑器…" />}><TextEditor path={path} value={value.text ?? ''} readOnly={!editing || operation.busy} onChange={setDraft} /></Suspense>}
      {value.kind === 'image' && <img className="max-w-full self-start rounded-md object-contain" alt={name} src={`data:${value.mime};base64,${value.image}`} />}
      {value.kind === 'binary' && <>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Button size="sm" variant="outline" disabled={!active || offset === 0 || preview.isFetching} onClick={() => setOffset(n => n - hexPage)}>上一段</Button>
          <Button size="sm" variant="outline" disabled={!active || offset + hexPage >= value.size || preview.isFetching} onClick={() => setOffset(n => n + hexPage)}>下一段</Button>
          <span className="text-xs text-muted-foreground">起始偏移 0x{offset.toString(16).toUpperCase()}</span>
        </div>
        <div className="min-h-28 flex-1 overflow-auto" aria-label="十六进制文件内容">
          <Suspense fallback={<QueryFeedback pending error={null} loading="正在加载十六进制编辑器…" />}><BinaryEditor key={offset} bytes={value.bytes ?? ''} /></Suspense>
        </div>
      </>}
    </>}
  </section>
}

function FilesContent({ sessionId, active }: { sessionId: string; active: boolean }) {
  const queryClient = useQueryClient()
  const [directory, setDirectory] = useState(sharedPath)
  const [showTrash, setShowTrash] = useState(false)
  const [editing, setEditing] = useState(false)
  const [preview, setPreview] = useState<string>()
  const [selection, setSelection] = useState<ReadonlySet<string>>(() => new Set())
  const selectionAnchor = useRef<string>(undefined)
  const treeRoot = useRef<HTMLUListElement>(null)
  const [pending, setPending] = useState<FileActionDraft>()
  const [uploadDirectory, setUploadDirectory] = useState(sharedPath)
  const [file, setFile] = useState<File>()
  const [overwrite, setOverwrite] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const filePane = useRef<HTMLDivElement>(null)
  const [wide, setWide] = useState(false)
  useLayoutEffect(() => {
    const node = filePane.current
    if (!node) return
    const observer = new ResizeObserver(([entry]) => {
      if (entry.contentRect.width > 0) setWide(entry.contentRect.width >= 600)
    })
    observer.observe(node)
    return () => observer.disconnect()
  }, [])
  const operation = useDeviceTransfer(sessionId)
  const actions = useDeviceFileActions(sessionId, changed)
  const busy = operation.busy || actions.busy
  const browse = active && !showTrash
  const shared = useDeviceDirectory(sessionId, sharedPath, browse)
  const listing = useDeviceDirectory(sessionId, directory, browse)
  const uploadListing = useDeviceDirectory(sessionId, uploadDirectory, browse && !!file)
  const data = listing.data
  const conflict = file && uploadListing.data?.entries.find(entry => entry.name === file.name)
  const blocked = !browse || busy || editing
  const canUpload = !blocked && !!uploadListing.data?.writable && !uploadListing.isError && !uploadListing.isFetching && (!conflict || conflict.kind === 'file' && overwrite)
  const select = (path: string, filePath?: string) => {
    setDirectory(path)
    setPreview(filePath)
  }
  function changed(paths: string[]) {
    if (!paths.length) return
    const affected = (value: string) => paths.some(path => value === path || value.startsWith(path + '/'))
    dropSelection(affected)
    const parent = paths.find(path => directory === path || directory.startsWith(path + '/'))
    if (parent) select(parentPath(parent))
    else if (preview && affected(preview)) setPreview(undefined)
    if (file && affected(uploadDirectory)) setFile(undefined)
  }
  const onAction = (action: FileMenuAction, path: string) => {
    switch (action) {
      case 'rename':
      case 'mkdir':
        setPending({ action, path })
        break
      case 'trash':
      case 'delete':
        setPending(undefined)
        actions.requestRemoval({ action, paths: [path] })
        break
      case 'upload':
        setFile(undefined)
        setUploadDirectory(path)
        setPending(undefined)
        if (fileInput.current) {
          fileInput.current.value = ''
          fileInput.current.click()
        }
        break
    }
  }
  const clearSelection = () => {
    selectionAnchor.current = undefined
    setSelection(new Set())
  }
  const dropSelection = (matches: (path: string) => boolean) => {
    if (selectionAnchor.current && matches(selectionAnchor.current)) selectionAnchor.current = undefined
    setSelection(current => new Set(Array.from(current).filter(path => !matches(path))))
  }
  const collapse = (path: string) => {
    dropSelection(value => value.startsWith(path + '/'))
  }
  const selectRow: TreeProps['selectRow'] = (event, path, selectable, activate) => {
    const toggle = event.ctrlKey || event.metaKey
    if (!event.shiftKey && !toggle) {
      selectionAnchor.current = selectable ? path : undefined
      setSelection(new Set(selectable ? [path] : []))
      activate()
      return
    }
    if (!selectable) return
    if (event.shiftKey) {
      // The expanded tree already defines the visible order; closed descendants are not mounted.
      const rows = treeRoot.current?.querySelectorAll<HTMLButtonElement>('button[data-selectable-file]')
      const visible = rows ? Array.from(rows, row => row.title) : []
      const first = visible.indexOf(selectionAnchor.current ?? '')
      const last = visible.indexOf(path)
      if (first >= 0 && last >= 0) {
        setSelection(new Set(visible.slice(Math.min(first, last), Math.max(first, last) + 1)))
        return
      }
    }
    selectionAnchor.current = path
    setSelection(current => {
      if (!toggle) return new Set([path])
      const next = new Set(current)
      if (!next.delete(path)) next.add(path)
      return next
    })
  }
  const upload = async () => {
    if (!file || !canUpload || operation.busy) return
    if (await operation.start({ file, purpose: 'file', targetPath: childPath(uploadDirectory, file.name), overwrite })) {
      setFile(undefined)
      void queryClient.invalidateQueries({ queryKey: ['device-directory', sessionId, uploadDirectory], exact: true })
    }
  }
  const tree = { sessionId, selected: preview ?? directory, selection, selectRow, collapse, active: browse, busy: blocked, select, onAction }
  return <div ref={filePane} className="min-h-0 flex flex-1 flex-col">
    <div className="flex shrink-0 items-center gap-2 border-b px-2 py-1">
      {showTrash ? <span className="flex-1 text-xs">共享存储</span>
        : <span className="min-w-0 flex-1 truncate text-xs" title={directory} aria-label="当前目录">{directory}</span>}
      {!showTrash && data && !data.writable && <Badge variant="secondary">只读</Badge>}
      <Toggle variant="outline" size="sm" pressed={showTrash} disabled={!active || busy || editing} onPressedChange={setShowTrash}><Trash2 data-icon="inline-start" />回收站</Toggle>
    </div>
    {actions.error && <div className="shrink-0 px-2 py-1"><Alert variant="destructive"><AlertDescription>{actions.error}</AlertDescription></Alert></div>}
    {showTrash && <DeviceTrash sessionId={sessionId} active={active} operation={actions} />}
    <FileRemovalDialog operation={actions} disabled={!active || operation.busy || editing} />
    <div hidden={showTrash} className="min-h-0 flex-1">
      <ResizablePanelGroup orientation={wide ? 'horizontal' : 'vertical'}>
        <ResizablePanel id="file-tree" defaultSize={wide ? '46%' : '55%'} minSize={wide ? '210px' : '120px'}>
          <div className="flex h-full min-h-0 min-w-0 flex-col">
            <div className="flex h-9 shrink-0 items-center gap-1 overflow-hidden border-b px-2" role="toolbar" aria-label="文件树操作">
              <Button size="icon-sm" variant="ghost" aria-label="新建目录" title="新建目录" className="shrink-0"
                disabled={blocked || !data?.writable || listing.isError} onClick={() => onAction('mkdir', directory)}><FolderPlus /></Button>
              <Button size="icon-sm" variant="ghost" aria-label="上传到当前目录" title="上传到当前目录" className="shrink-0"
                disabled={blocked || !data?.writable || listing.isError} onClick={() => onAction('upload', directory)}><UploadIcon /></Button>
              <Button size="icon-sm" variant="ghost" aria-label="刷新当前目录" title="刷新当前目录" className="shrink-0"
                disabled={!browse || busy || listing.isFetching} onClick={() => void listing.refetch()}><RefreshCw /></Button>
              {selection.size > 1 && <div className="ml-auto flex min-w-0 items-center gap-0.5" aria-label="批量文件操作" role="group">
                <span className="truncate px-1 text-xs tabular-nums" title={`已选 ${selection.size} 项`}>{selection.size}</span>
                <Button size="icon-xs" variant="ghost" aria-label="批量移入回收站" title="批量移入回收站" className="shrink-0"
                  disabled={blocked} onClick={() => { setPending(undefined); actions.requestRemoval({ action: 'trash', paths: batchTargets(selection), batch: true }) }}><Trash2 /></Button>
                <Button size="icon-xs" variant="ghost" aria-label="批量永久删除" title="批量永久删除" className="shrink-0 text-destructive"
                  disabled={blocked} onClick={() => { setPending(undefined); actions.requestRemoval({ action: 'delete', paths: batchTargets(selection), batch: true }) }}><CircleX /></Button>
                <Button size="icon-xs" variant="ghost" aria-label="清除选择" title="清除选择" className="shrink-0"
                  disabled={blocked} onClick={clearSelection}><X /></Button>
              </div>}
            </div>
            {pending && <div className="shrink-0 px-2 py-1">
              <FileActionPrompt key={pending.action + pending.path}
                pending={pending} setPending={setPending} disabled={blocked} operation={actions} changed={changed} />
            </div>}
            <div className="min-h-0 flex-1 overflow-auto px-1 py-2">
              <ul ref={treeRoot} aria-label="手机文件树">
                <DirectoryBranch {...tree} path={sharedPath} name="共享存储" root />
                {shared.data?.rootAvailable && <DirectoryBranch {...tree} path="/" name="根目录" />}
              </ul>
            </div>
            <input type="file" ref={fileInput} hidden aria-label="选择本机文件" onChange={event => {
              setFile(event.currentTarget.files?.[0])
              setOverwrite(false)
            }} />
            {file && <form className="shrink-0 border-t p-2" onSubmit={event => { event.preventDefault(); void upload() }}>
              <FieldGroup className="gap-2">
                <p className="truncate" title={childPath(uploadDirectory, file.name)}>{file.name} → {uploadDirectory}</p>
                <QueryFeedback pending={uploadListing.isPending} error={uploadListing.error} />
                {uploadListing.data && !uploadListing.data.writable && <p className="text-muted-foreground">目标目录只读</p>}
                {conflict?.kind === 'file' && <Toggle variant="outline" pressed={overwrite} onPressedChange={setOverwrite} disabled={busy}>确认覆盖同名文件</Toggle>}
                {conflict && conflict.kind !== 'file' && <p className="text-destructive">同名项目不是普通文件，不能覆盖。</p>}
                <div className="flex gap-1">
                  <Button type="submit" size="sm" disabled={!canUpload}>上传</Button>
                  <Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => setFile(undefined)}>取消</Button>
                </div>
              </FieldGroup>
            </form>}
          </div>
        </ResizablePanel>
        {preview && <ResizableHandle withHandle aria-label={wide ? '调整文件树宽度' : '调整文件树高度'} />}
        {preview && <ResizablePanel id="file-preview" minSize={wide ? '220px' : '160px'}>
          <DeviceFilePreview key={preview} sessionId={sessionId} path={preview} active={browse} writable={!!data?.writable && !listing.isError}
            editing={editing} setEditing={setEditing} operation={operation} onSaved={() => void listing.refetch()} />
        </ResizablePanel>}
      </ResizablePanelGroup>
    </div>
    <div className="shrink-0 px-2"><TransferFeedback operation={operation} /></div>
  </div>
}

const AppList = memo(function AppList({ groups, search, selected, busy, onSelect }: {
  groups: AppGroup[]
  search: string
  selected: string[]
  busy: boolean
  onSelect: (packageName: string, pressed: boolean) => void
}) {
  const scrollArea = useRef<HTMLDivElement>(null)
  const scrollContent = useRef<HTMLDivElement>(null)
  const results = useMemo(() => groups.flatMap(group => {
    const apps = search ? group.apps.filter(app => app.name?.toLowerCase().includes(search) || app.packageName.toLowerCase().includes(search)) : group.apps
    return apps.length ? [{ initial: group.initial, apps }] : []
  }), [groups, search])
  return <div className="flex min-h-0 flex-1 gap-1" aria-label="已安装应用" role="region">
    <div ref={scrollArea} className="min-w-0 flex-1 overflow-auto">
      <div ref={scrollContent} className="relative flow-root">
    {!results.length && <p className="px-1 py-3 text-muted-foreground">没有匹配的应用</p>}
    {results.map(group => <section key={group.initial} data-app-letter={group.initial} aria-label={`${group.initial} 组`} className="mb-3">
      <h3 className="sticky top-0 bg-card px-1 py-1 text-xs font-medium text-muted-foreground">{group.initial}</h3>
      <ul>
        {group.apps.map(item => {
          const pressed = selected.includes(item.packageName)
          return <li key={item.packageName} className="flex min-w-0 items-center gap-2 border-b px-1 py-1.5 text-sm">
            {item.icon ? <img alt="" loading="lazy" src={appIconSource(item.icon)} className="size-8 shrink-0 rounded-sm object-contain" />
              : <Package className="size-8 shrink-0 p-1.5 text-muted-foreground" aria-label="无应用图标" />}
            <div className="min-w-0 flex-1">
              <div className="truncate font-medium" title={item.name}>{item.name || '无应用名称'}</div>
              <div className="truncate text-xs text-muted-foreground" title={item.packageName}>{item.packageName}</div>
            </div>
            {item.thirdParty ? <Toggle variant="outline" size="sm" pressed={pressed} aria-label={`选择 ${item.packageName}`} disabled={busy || selected.length >= batchLimit && !pressed} onPressedChange={next => onSelect(item.packageName, next)}>{pressed ? '已选择' : '选择'}</Toggle>
              : <Badge variant="secondary">系统应用</Badge>}
          </li>
        })}
      </ul>
    </section>)}
      </div>
    </div>
    <AppLetterIndex scroller={scrollArea} content={scrollContent} groups={results} />
  </div>
})

function AppsContent({ sessionId, active }: { sessionId: string; active: boolean }) {
  const queries = useQueryClient()
  const [appScope, setAppScope] = useState<'user' | 'all'>('user')
  const [search, setSearch] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [confirmation, setConfirmation] = useState<{ userId: number; packages: string[] }>()
  const [results, setResults] = useState<Removal[]>([])
  const [removing, setRemoving] = useState(false)
  const removalController = useRef<AbortController | null>(null)
  useEffect(() => () => removalController.current?.abort(), [])
  const [apk, setAPK] = useState<File>()
  const [inputVersion, setInputVersion] = useState(0)
  const operation = useDeviceTransfer(sessionId)
  const busy = removing || operation.busy
  const listing = useQuery({
    ...deviceAppsQuery(sessionId, appScope),
    enabled: active,
  })
  const data = listing.data
  const displayData = useDeferredValue(active ? data : undefined)
  const packageFilter = useDeferredValue(search.trim().toLowerCase())
  const groups = displayData?.groups
  const changeAppSelection = useCallback((packageName: string, pressed: boolean) => {
    setSelected(list => pressed ? [...list, packageName] : list.filter(name => name !== packageName))
  }, [])
  const picked = useMemo(() => data?.apps.filter(item => item.thirdParty && selected.includes(item.packageName)).map(item => item.packageName) ?? [], [data, selected])
  const install = async () => {
    if (!active || !apk || busy) return
    if (await operation.start({ file: apk, purpose: 'apk' })) {
      setAPK(undefined); setInputVersion(n => n + 1)
      void queries.invalidateQueries({ queryKey: ['device-apps', sessionId] })
    }
  }
  const remove = async () => {
    if (!active || !confirmation || removalController.current || busy) return
    const controller = new AbortController()
    removalController.current = controller
    setRemoving(true); setResults([]); setSelected([]); setConfirmation(undefined)
    try {
      const { results: got } = await api<{ results: Removal[] }>(`${sessionPath(sessionId)}/apps/uninstall`, { method: 'POST', body: confirmation, signal: controller.signal })
      setResults(got)
      if (got.some(item => item.state !== 'succeeded')) toast.add({ title: '卸载已结束，请核对逐项结果', type: 'error' })
    } catch (error) {
      if (!controller.signal.aborted) {
        const message = errorMessage(error)
        const state = error instanceof ApiError && error.code !== 'RESULT_UNKNOWN' && error.status >= 400 && error.status < 500 ? 'not_started' : 'unknown'
        setResults(confirmation.packages.map(packageName => ({ packageName, state, message })))
        toast.add({ title: '卸载未确认', description: message, type: 'error', timeout: 0 })
      }
    } finally {
      removalController.current = null
      if (!controller.signal.aborted) {
        setRemoving(false)
        void queries.invalidateQueries({ queryKey: ['device-apps', sessionId] })
      }
    }
  }
  return <div className="flex min-h-0 flex-1 flex-col gap-2 p-2">
    <form onSubmit={event => { event.preventDefault(); void install() }}>
      <FieldGroup className="flex-wrap items-end gap-2">
        <Field>
          <FieldLabel htmlFor="device-apk-input">安装应用（APK）</FieldLabel>
          <Input key={inputVersion} id="device-apk-input" type="file" accept=".apk,application/vnd.android.package-archive" disabled={!active || busy} onChange={event => setAPK(event.currentTarget.files?.[0])} />
        </Field>
        {apk && <Button type="submit" size="sm" disabled={!active || busy || !apk.name.toLowerCase().endsWith('.apk')}>安装到手机</Button>}
      </FieldGroup>
    </form>
    <TransferFeedback operation={operation} />
    <ToggleGroup value={[appScope]} variant="outline" size="sm" spacing={0} aria-label="应用范围" onValueChange={values => {
      const value = values[0]
      if (value === 'user' || value === 'all') setAppScope(value)
    }}>
      <ToggleGroupItem value="user">用户应用</ToggleGroupItem>
      <ToggleGroupItem value="all">全部应用</ToggleGroupItem>
    </ToggleGroup>
    <div className="flex flex-wrap items-center gap-2">
      <Input className="min-w-28 flex-1" aria-label="按应用名或包名筛选" placeholder="应用名或包名" value={search} onChange={event => setSearch(event.currentTarget.value)} />
      <Button variant="outline" size="sm" aria-label="刷新应用列表" disabled={busy || listing.isFetching} onClick={() => void listing.refetch()}><RefreshCw /></Button>
      <Button variant="destructive" size="sm" disabled={!active || !data || !picked.length || busy || listing.isFetching || listing.isError} onClick={() => { if (data) setConfirmation({ userId: data.userId, packages: picked }) }}>卸载所选（{picked.length}）</Button>
    </div>
    <QueryFeedback pending={!listing.isError && (!data || data !== displayData)} error={listing.error} loading={active ? '正在加载应用和图标…' : '等待设备控制连接'} />
    {active && data === displayData && groups && !listing.isError && <AppList groups={groups} search={packageFilter} selected={picked} busy={busy} onSelect={changeAppSelection} />}
    {removing && <p role="status" className="text-sm">正在逐项卸载并核对结果…</p>}
    {!!results.length && <ul className="max-h-[20vh] overflow-y-auto text-sm" aria-label="逐项卸载结果">{results.map(item => <li className="break-all" key={item.packageName}>{item.packageName}：{removalLabels[item.state]}{item.message && ` · ${item.message}`}</li>)}</ul>}
    {confirmation && <section className="shrink-0 rounded-md border border-destructive p-2" aria-label="确认卸载">
      <h3 className="font-medium">确认卸载 {confirmation.packages.length} 个应用</h3>
      <p className="text-xs text-muted-foreground">Android 用户 {confirmation.userId} 中的应用数据可能被删除。</p>
      <ul className="my-1 max-h-24 overflow-auto break-all text-xs">{confirmation.packages.map(pkg => <li key={pkg}>{pkg}</li>)}</ul>
      <div className="flex justify-end gap-2"><Button variant="outline" size="sm" onClick={() => setConfirmation(undefined)}>取消</Button><Button variant="destructive" size="sm" onClick={() => void remove()}>确认卸载</Button></div>
    </section>}
  </div>
}

export function DeviceManagement({ sessionId, active, children }: { sessionId?: string; active: boolean; children: ReactNode }) {
  const [panel, setPanel] = useState('files')
  const waiting = <p role="status" className="p-3 text-muted-foreground">等待设备控制连接</p>
  const panelClass = 'flex min-h-0 flex-1 flex-col overflow-hidden [&[hidden]]:hidden'
  return <Tabs value={panel} onValueChange={setPanel} className="min-h-0 flex-1 gap-0">
    <TabsList variant="line" className="w-full shrink-0 border-b px-1" aria-label="工作台工具">
      <TabsTrigger value="files"><FolderOpen data-icon="inline-start" />文件</TabsTrigger>
      <TabsTrigger value="apps"><Package data-icon="inline-start" />应用</TabsTrigger>
      <TabsTrigger value="clipboard"><Clipboard data-icon="inline-start" />剪贴板</TabsTrigger>
    </TabsList>
    <TabsContent value="files" keepMounted className={panelClass}>
      {sessionId ? <FilesContent key={sessionId} sessionId={sessionId} active={active && panel === 'files'} /> : waiting}
    </TabsContent>
    <TabsContent value="apps" keepMounted className={panelClass}>
      {sessionId ? <AppsContent key={sessionId} sessionId={sessionId} active={active && panel === 'apps'} /> : waiting}
    </TabsContent>
    <TabsContent value="clipboard" className="min-h-0 flex-1 overflow-auto">{children}</TabsContent>
  </Tabs>
}
