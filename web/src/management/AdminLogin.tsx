import { useState, type FormEvent } from 'react'
import { KeyRound, ShieldCheck } from 'lucide-react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { useAuth } from '@/lib/auth'
import { errorMessage } from '@/lib/api'

export function AdminLogin() {
  const { login, error: restoreError } = useAuth()
  const [token, setToken] = useState('')
  const [failure, setFailure] = useState('')
  const [pending, setPending] = useState(false)

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!token.trim() || pending) return
    setPending(true)
    setFailure('')
    try {
      await login(token.trim())
      setToken('')
    } catch (error) {
      setFailure(errorMessage(error))
    } finally {
      setPending(false)
    }
  }

  return (
    <main className="grid min-h-svh place-items-center px-4 py-10">
      <Card className="w-full max-w-md">
        <CardHeader>
          <div className="mb-2 flex size-10 items-center justify-center rounded-xl bg-primary text-primary-foreground"><ShieldCheck /></div>
          <CardTitle>管理员登录</CardTitle>
          <CardDescription>使用管理员访问令牌建立安全会话。</CardDescription>
        </CardHeader>
        <CardContent>
          {(failure || restoreError) && <Alert variant="destructive" className="mb-4"><KeyRound /><AlertTitle>无法登录</AlertTitle><AlertDescription>{failure || restoreError}</AlertDescription></Alert>}
          <form onSubmit={submit}>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="admin-token">管理员令牌</FieldLabel>
                <Input id="admin-token" type="password" autoComplete="current-password" value={token} onChange={event => setToken(event.target.value)} aria-invalid={Boolean(failure)} required />
                <FieldDescription>请勿将令牌放入 URL、普通日志或共享截图。</FieldDescription>
              </Field>
              <Button type="submit" disabled={pending || !token.trim()}>{pending ? '正在登录…' : '登录'}</Button>
            </FieldGroup>
          </form>
        </CardContent>
      </Card>
    </main>
  )
}
