'use client'

import { FormEvent, useState } from 'react'
import { getPharmacyAppUrl } from '@/lib/app-links'

type RegisterForm = {
  companyName: string
  companyEmail: string
  firstName: string
  lastName: string
  email: string
  password: string
  confirmPassword: string
}

const initialForm: RegisterForm = {
  companyName: '',
  companyEmail: '',
  firstName: '',
  lastName: '',
  email: '',
  password: '',
  confirmPassword: '',
}

export default function RegisterPage() {
  const [form, setForm] = useState(initialForm)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  function update(field: keyof RegisterForm, value: string) {
    setForm((current) => ({ ...current, [field]: value }))
  }

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setError('')

    if (form.password !== form.confirmPassword) {
      setError('كلمتا المرور غير متطابقتين')
      return
    }

    setLoading(true)
    try {
      const response = await fetch('/api/v1/auth/register', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          company_name: form.companyName,
          company_email: form.companyEmail,
          first_name: form.firstName,
          last_name: form.lastName,
          email: form.email,
          password: form.password,
        }),
      })
      const body = await response.json().catch(() => ({}))
      if (!response.ok) throw new Error(body.message || 'تعذر إنشاء الحساب')
      window.location.assign(getPharmacyAppUrl())
    } catch (registrationError) {
      setError(registrationError instanceof Error ? registrationError.message : 'تعذر إنشاء الحساب')
      setLoading(false)
    }
  }

  return (
    <main className="auth-shell" dir="rtl">
      <div className="auth-orb auth-orb-one" />
      <div className="auth-orb auth-orb-two" />
      <section className="auth-card" aria-labelledby="register-title">
        <div className="auth-brand">
          <span className="brand-mark" aria-hidden="true"><span /><span /><span /></span>
          <span className="brand-name"><strong>Pharmacy</strong><em>OS</em></span>
        </div>
        <div className="auth-heading">
          <p className="eyebrow">ابدأ في دقائق</p>
          <h1 id="register-title">أنشئ حساب صيدليتك</h1>
          <p>بيانات بسيطة، ثم نجهز لك مساحة العمل والفرع الأول تلقائيًا.</p>
        </div>

        <form className="register-form" onSubmit={handleSubmit}>
          {error && <p className="form-error" role="alert">{error}</p>}
          <label><span>اسم الصيدلية</span><input required minLength={2} value={form.companyName} onChange={(e) => update('companyName', e.target.value)} placeholder="صيدلية الشفاء" /></label>
          <label><span>بريد الصيدلية</span><input required type="email" value={form.companyEmail} onChange={(e) => update('companyEmail', e.target.value)} placeholder="pharmacy@example.com" /></label>
          <div className="form-grid">
            <label><span>الاسم الأول</span><input required value={form.firstName} onChange={(e) => update('firstName', e.target.value)} placeholder="أحمد" /></label>
            <label><span>اسم العائلة</span><input required value={form.lastName} onChange={(e) => update('lastName', e.target.value)} placeholder="محمد" /></label>
          </div>
          <label><span>بريد صاحب الصيدلية</span><input required type="email" autoComplete="email" value={form.email} onChange={(e) => update('email', e.target.value)} placeholder="owner@example.com" /></label>
          <div className="form-grid">
            <label><span>كلمة المرور</span><input required minLength={10} type="password" autoComplete="new-password" value={form.password} onChange={(e) => update('password', e.target.value)} placeholder="10 أحرف أو أكثر" /></label>
            <label><span>تأكيد كلمة المرور</span><input required minLength={10} type="password" autoComplete="new-password" value={form.confirmPassword} onChange={(e) => update('confirmPassword', e.target.value)} placeholder="أعد كتابة كلمة المرور" /></label>
          </div>
          <p className="form-hint">استخدم حرفًا كبيرًا وصغيرًا ورقمًا ورمزًا خاصًا.</p>
          <button className="button button-primary auth-submit" disabled={loading} type="submit">
            {loading ? 'جاري إنشاء الحساب...' : 'إنشاء الحساب والبدء'}
            <span aria-hidden="true">←</span>
          </button>
        </form>
        <p className="auth-footer">لديك حساب بالفعل؟ <a href={`${getPharmacyAppUrl()}/login`}>الدخول إلى التطبيق</a></p>
        <p className="auth-note">بعد الإنشاء سيتم تسجيل دخولك تلقائيًا ونقلك إلى تطبيق الصيدلية.</p>
      </section>
    </main>
  )
}