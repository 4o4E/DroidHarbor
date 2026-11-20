import { useEffect, useRef } from 'react'
import { editor, Uri } from 'monaco-editor/editor/editor.api.js'
import EditorWorker from 'monaco-editor/editor/editor.worker.js?worker'
import JSONWorker from 'monaco-editor/languages/features/json/json.worker.js?worker'
import 'monaco-editor/basic-languages/monaco.contribution.js'
import 'monaco-editor/languages/features/json/register.js'

Object.assign(globalThis, { MonacoEnvironment: { getWorker: (_: string, label: string) =>
  label === 'json' ? new JSONWorker() : new EditorWorker() } })

export function TextEditor({ path, value, readOnly, onChange }: { path: string; value: string; readOnly: boolean; onChange: (text: string) => void }) {
  const container = useRef<HTMLDivElement>(null)
  const instance = useRef<editor.IStandaloneCodeEditor>(null)
  const update = useRef(onChange)
  useEffect(() => { update.current = onChange }, [onChange])
  useEffect(() => {
    if (!container.current) return
    const color = () => document.documentElement.classList.contains('dark') ? 'vs-dark' : 'vs'
    const model = editor.createModel(value, undefined, Uri.from({ scheme: 'inmemory', path }))
    const view = editor.create(container.current, {
      model, readOnly: true, domReadOnly: true, ariaLabel: '文本文件编辑器',
      automaticLayout: true, theme: color(), minimap: { enabled: false },
      wordWrap: 'on', scrollBeyondLastLine: false, folding: false, glyphMargin: false,
    })
    instance.current = view
    const theme = new MutationObserver(() => editor.setTheme(color()))
    theme.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
    const listener = model.onDidChangeContent(() => update.current(model.getValue(editor.EndOfLinePreference.TextDefined, true)))
    return () => { instance.current = null; theme.disconnect(); listener.dispose(); view.dispose(); model.dispose() }
  }, [path, value])
  useEffect(() => {
    const view = instance.current
    view?.updateOptions({ readOnly, domReadOnly: readOnly })
    if (!readOnly) view?.focus()
  }, [path, readOnly, value])
  return <div ref={container} className="min-h-36 flex-1" />
}
