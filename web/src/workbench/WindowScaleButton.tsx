import { Button } from '@/components/ui/button'
import { ContextMenu, ContextMenuContent, ContextMenuGroup, ContextMenuItem, ContextMenuTrigger } from '@/components/ui/context-menu'

export function WindowScaleButton({ scales, value, onChange, disabled }: { scales: readonly number[]; value: number; onChange: (scale: number) => void; disabled?: boolean }) {
 return <ContextMenu><ContextMenuTrigger render={<Button size="icon-sm" variant="ghost" className="text-[10px] tabular-nums" disabled={disabled} aria-label={`窗口倍率 ${value}×`} title="左键切换倍率，右键选择" onClick={() => { const index = scales.indexOf(value); onChange(scales[(index + 1) % scales.length]) }} />}>{value}×</ContextMenuTrigger><ContextMenuContent><ContextMenuGroup>{scales.map(scale => <ContextMenuItem key={scale} onClick={() => onChange(scale)}>{scale}×{scale === value ? ' · 当前' : ''}</ContextMenuItem>)}</ContextMenuGroup></ContextMenuContent></ContextMenu>
}
