import { useLayoutEffect, useState, type RefObject } from 'react'
import { Button } from '@/components/ui/button'
import type { AppGroup } from './DeviceAppSort'

type Mark = { letter: string; top: number; offset: number }

export function AppLetterIndex({ scroller, content, groups }: {
  scroller: RefObject<HTMLDivElement | null>
  content: RefObject<HTMLDivElement | null>
  groups: readonly AppGroup[]
}) {
  const [marks, setMarks] = useState<Mark[]>([])
  useLayoutEffect(() => {
    const viewport = scroller.current
    const list = content.current
    if (!viewport || !list) return
    const measure = () => {
      const sections = list.querySelectorAll<HTMLElement>('[data-app-letter]')
      const railHeight = Math.max(1, viewport.clientHeight - 16)
      const contentHeight = Math.max(1, list.offsetHeight)
      // Keep dense neighboring groups clickable without reordering their positions.
      const spacing = Math.min(16, railHeight / Math.max(1, sections.length - 1))
      let previous = -spacing
      setMarks(Array.from(sections, (section, index) => {
        const offset = section.offsetTop
        const ceiling = railHeight - (sections.length - index - 1) * spacing
        const position = Math.max(previous + spacing, Math.min(ceiling, offset / contentHeight * railHeight))
        previous = position
        return { letter: section.dataset.appLetter!, top: position / railHeight * 100, offset }
      }))
    }
    const observer = new ResizeObserver(measure)
    observer.observe(viewport)
    observer.observe(list)
    measure()
    return () => observer.disconnect()
  }, [content, scroller, groups])

  if (!marks.length) return null
  return <nav aria-label="应用首字母导航" className="relative my-2 w-6 shrink-0 border-l">
    {marks.map(mark => <Button key={mark.letter} size="icon-sm" variant="ghost"
      className="absolute inset-x-0 h-4 w-auto -translate-y-1/2 rounded-sm px-0 text-[10px] text-muted-foreground"
      aria-label={`跳到 ${mark.letter} 组`} title={`${mark.letter} 组`} style={{ top: `${mark.top}%` }}
      onClick={() => scroller.current?.scrollTo({ top: mark.offset })}>{mark.letter}</Button>)}
  </nav>
}
