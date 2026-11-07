import { lazy, Suspense } from 'react'

const CompactWorkbench = lazy(() => import('./workbench/CompactWorkbench').then(module => ({ default: module.CompactWorkbench })))
const ManagementApp = lazy(() => import('./management/ManagementApp').then(module => ({ default: module.ManagementApp })))

export default function App() {
  const params = new URLSearchParams(window.location.search)
  return <Suspense fallback={<main className="grid min-h-svh place-items-center text-sm text-muted-foreground">正在打开页面…</main>}>
    {params.get('view') === 'compact'
      ? <CompactWorkbench initialDeviceId={params.get('device')} initialMode={params.get('mode')} />
      : <ManagementApp />}
  </Suspense>
}
