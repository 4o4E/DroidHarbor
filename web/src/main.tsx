import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { AuthProvider } from '@/lib/auth'
import { Toaster } from '@/components/ui/toast'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Toaster><AuthProvider><App /></AuthProvider></Toaster>
  </StrictMode>,
)
