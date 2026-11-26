import { toast } from '@/components/ui/toast'
import { errorMessage } from './api'

export async function copyToComputer(text: string) {
  try {
    if (window.isSecureContext && navigator.clipboard) {
      await navigator.clipboard.writeText(text)
    } else {
      // LAN HTTP still supports Chromium's user-gesture copy command.
      const active = document.activeElement
      const field = document.createElement('textarea')
      field.value = text
      field.readOnly = true
      field.className = 'fixed top-0 left-0 size-px opacity-0 pointer-events-none'
      document.body.append(field)
      try {
        field.select()
        if (!document.execCommand('copy')) throw new Error('浏览器拒绝复制到系统剪贴板')
      } finally {
        field.remove()
        if (active instanceof HTMLElement) active.focus({ preventScroll: true })
      }
    }
    toast.add({ title: '已复制到电脑剪贴板', type: 'success' })
  } catch (error) {
    toast.add({ title: '复制到电脑未完成', description: errorMessage(error), type: 'error' })
  }
}
