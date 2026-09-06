'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import { authApi } from '@/lib/api'

export default function VerifyEmailPage() {
  const [message, setMessage] = useState('جاري تأكيد البريد الإلكتروني...')
  const [error, setError] = useState(false)
  const [email, setEmail] = useState('')
  const [hasToken, setHasToken] = useState(false)
  const [sending, setSending] = useState(false)
  const [sent, setSent] = useState(false)

  useEffect(() => {
    const params = new URLSearchParams(window.location.search)
    const token = params.get('token')
    const pendingEmail = params.get('email')?.trim() || ''
    setEmail(pendingEmail)
    setHasToken(Boolean(token))

    if (!token) {
      if (pendingEmail) {
        setMessage('تحقق من بريدك الإلكتروني لتفعيل الحساب')
      } else {
        setError(true)
        setMessage('رابط تأكيد البريد غير صالح')
      }
      return
    }

    fetch('/api/v1/auth/verify-email', {
      method: 'POST',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ token }),
    })
      .then(async (response) => {
        const body = await response.json().catch(() => ({}))
        if (!response.ok) throw new Error(body.message || 'تعذر تأكيد البريد')
        setMessage('تم تأكيد البريد بنجاح. جاري فتح التطبيق...')
        window.setTimeout(() => window.location.assign('/login'), 900)
      })
      .catch((verificationError) => {
        setError(true)
        setMessage(verificationError instanceof Error ? verificationError.message : 'تعذر تأكيد البريد')
      })
  }, [])

  async function resendVerification() {
    if (!email || sending) return
    setSending(true)
    setError(false)
    setSent(false)
    try {
      await authApi.resendVerification(email)
      setSent(true)
      setMessage('تم إرسال رابط تحقق جديد إلى بريدك الإلكتروني')
    } catch (resendError) {
      setError(true)
      setMessage(resendError instanceof Error ? resendError.message : 'تعذر إرسال رابط التحقق')
    } finally {
      setSending(false)
    }
  }

  return (
    <main className="relative flex min-h-screen items-center justify-center overflow-hidden bg-background px-4 py-8" dir="rtl">
      <div className="absolute -right-32 -top-32 h-96 w-96 rounded-full bg-primary/10 blur-3xl" />
      <div className="absolute -bottom-40 -left-24 h-96 w-96 rounded-full bg-emerald-400/10 blur-3xl" />
      <section className="relative w-full max-w-lg rounded-3xl border border-border bg-card p-8 text-center shadow-2xl sm:p-12" aria-live="polite">
        <div className={`mx-auto flex h-16 w-16 items-center justify-center rounded-2xl text-2xl font-black ${error ? 'bg-destructive/10 text-destructive' : 'bg-primary/10 text-primary'}`}>
          {error ? '!' : '✓'}
        </div>
        <p className="mt-6 text-sm font-medium text-primary">Pharmacy OS</p>
        <h1 className="mt-3 text-2xl font-bold leading-relaxed">{message}</h1>
        {!hasToken && email && (
          <div className="mt-8 space-y-3">
            <button
              className="inline-flex h-11 items-center rounded-xl bg-primary px-6 text-sm font-semibold text-primary-foreground shadow-lg shadow-primary/20 transition hover:bg-primary/90 disabled:cursor-not-allowed disabled:opacity-60"
              type="button"
              onClick={resendVerification}
              disabled={sending}
            >
              {sending ? 'جاري الإرسال...' : 'إعادة إرسال رابط التحقق'}
            </button>
            {sent && <p className="text-sm text-emerald-600">تحقق من صندوق الوارد ومجلد الرسائل غير المرغوب فيها.</p>}
          </div>
        )}
        <Link className="mt-8 inline-flex h-11 items-center rounded-xl border border-border px-6 text-sm font-semibold transition hover:bg-muted" href="/login">
          العودة إلى تسجيل الدخول
        </Link>
      </section>
    </main>
  )
}