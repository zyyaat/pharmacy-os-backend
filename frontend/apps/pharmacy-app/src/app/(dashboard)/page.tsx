'use client'

import Link from 'next/link'
import { Badge, Icon } from '@/components/ui'

const stats = [
  { label: 'إجمالي المنتجات', value: '1,248', change: '+8.4%', note: 'منذ الشهر الماضي', icon: 'package' as const, tone: 'primary' },
  { label: 'منتجات منخفضة المخزون', value: '27', change: '-5.2%', note: 'مقارنة بالأسبوع الماضي', icon: 'alert' as const, tone: 'warning' },
  { label: 'مبيعات اليوم', value: '18,450 ج.م', change: '+12.8%', note: 'عن نفس اليوم السابق', icon: 'chart' as const, tone: 'success' },
  { label: 'الحضور اليوم', value: '18 / 24', change: '75%', note: 'من إجمالي الموظفين', icon: 'users' as const, tone: 'info' },
]

const sales = [
  { id: '#INV-1048', customer: 'محمد علي', items: '3 أصناف', total: '245.00 ج.م', time: 'منذ 8 دقائق', status: 'مكتملة' },
  { id: '#INV-1047', customer: 'صيدلية الفرع الثاني', items: '12 صنف', total: '1,840.00 ج.م', time: 'منذ 26 دقيقة', status: 'مكتملة' },
  { id: '#INV-1046', customer: 'سارة محمود', items: '2 صنف', total: '96.50 ج.م', time: 'منذ 42 دقيقة', status: 'قيد المراجعة' },
]

const lowStock = [
  { name: 'أوجمنتين 1 جم', category: 'مضاد حيوي', quantity: 4, limit: 20 },
  { name: 'كونجستال أقراص', category: 'أدوية البرد', quantity: 8, limit: 25 },
  { name: 'فنتولين بخاخ', category: 'أدوية الجهاز التنفسي', quantity: 11, limit: 30 },
]

const toneClasses = {
  primary: 'bg-primary/10 text-primary',
  warning: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
  success: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  info: 'bg-blue-500/10 text-blue-600 dark:text-blue-400',
}

export default function DashboardPage() {
  return (
    <div className="mx-auto max-w-[1500px] space-y-7 animate-fade-in">
      <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-center">
        <div>
          <div className="mb-2 flex items-center gap-2 text-xs text-muted-foreground">
            <span>الرئيسية</span>
            <Icon name="chevronLeft" size={13} />
            <span className="text-primary">لوحة التحكم</span>
          </div>
          <h1 className="text-2xl font-bold tracking-tight lg:text-3xl">لوحة التحكم</h1>
          <p className="mt-2 text-sm text-muted-foreground">نظرة سريعة على أداء صيدليات الأمل اليوم</p>
        </div>
        <div className="flex items-center gap-3">
          <button className="hidden rounded-xl border border-border bg-card px-4 py-2.5 text-sm font-medium transition-colors hover:bg-accent sm:block">
            آخر 7 أيام <span className="mr-2 text-muted-foreground">⌄</span>
          </button>
          <Link href="/inventory" className="flex items-center gap-2 rounded-xl bg-primary px-4 py-2.5 text-sm font-semibold text-primary-foreground shadow-lg shadow-primary/20 transition-all hover:-translate-y-0.5 hover:bg-primary/90">
            <Icon name="plus" size={17} />
            إضافة منتج
          </Link>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {stats.map((stat) => (
          <div key={stat.label} className="group relative overflow-hidden rounded-2xl border border-border bg-card p-5 shadow-sm transition-all hover:-translate-y-0.5 hover:shadow-md">
            <div className="flex items-start justify-between">
              <div>
                <p className="text-sm text-muted-foreground">{stat.label}</p>
                <p className="mt-3 text-2xl font-bold tracking-tight">{stat.value}</p>
              </div>
              <div className={`rounded-xl p-3 ${toneClasses[stat.tone as keyof typeof toneClasses]}`}>
                <Icon name={stat.icon} size={21} />
              </div>
            </div>
            <div className="mt-4 flex items-center gap-1.5 text-xs">
              <span className={stat.tone === 'warning' ? 'text-amber-600 dark:text-amber-400' : 'text-emerald-600 dark:text-emerald-400'}>
                {stat.change}
              </span>
              <span className="text-muted-foreground">{stat.note}</span>
            </div>
            <div className="absolute -bottom-8 -left-8 h-24 w-24 rounded-full bg-primary/5 blur-2xl transition-all group-hover:scale-150" />
          </div>
        ))}
      </div>

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-[1.55fr_1fr]">
        <section className="overflow-hidden rounded-2xl border border-border bg-card shadow-sm">
          <div className="flex items-center justify-between border-b border-border px-5 py-5">
            <div>
              <h2 className="font-semibold">آخر المبيعات</h2>
              <p className="mt-1 text-xs text-muted-foreground">آخر العمليات المسجلة في الفرع</p>
            </div>
            <Link href="/reports" className="text-sm font-medium text-primary hover:underline">عرض التقارير</Link>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[580px] text-right text-sm">
              <thead className="bg-muted/45 text-xs text-muted-foreground">
                <tr>
                  <th className="px-5 py-3 font-medium">رقم الفاتورة</th>
                  <th className="px-5 py-3 font-medium">العميل</th>
                  <th className="px-5 py-3 font-medium">الإجمالي</th>
                  <th className="px-5 py-3 font-medium">الحالة</th>
                  <th className="px-5 py-3 font-medium">الوقت</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {sales.map((sale) => (
                  <tr key={sale.id} className="transition-colors hover:bg-muted/30">
                    <td className="px-5 py-4 font-semibold text-primary">{sale.id}</td>
                    <td className="px-5 py-4">
                      <p className="font-medium">{sale.customer}</p>
                      <p className="mt-1 text-xs text-muted-foreground">{sale.items}</p>
                    </td>
                    <td className="px-5 py-4 font-semibold">{sale.total}</td>
                    <td className="px-5 py-4">
                      <Badge variant={sale.status === 'مكتملة' ? 'success' : 'warning'}>{sale.status}</Badge>
                    </td>
                    <td className="px-5 py-4 text-xs text-muted-foreground">{sale.time}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <section className="rounded-2xl border border-border bg-card p-5 shadow-sm">
          <div className="flex items-start justify-between">
            <div>
              <h2 className="font-semibold">يحتاج إلى انتباه</h2>
              <p className="mt-1 text-xs text-muted-foreground">منتجات تقترب من حد إعادة الطلب</p>
            </div>
            <Link href="/inventory" className="text-sm font-medium text-primary hover:underline">كل المخزون</Link>
          </div>
          <div className="mt-5 space-y-4">
            {lowStock.map((product) => (
              <div key={product.name} className="rounded-xl border border-border/80 p-3.5">
                <div className="flex items-center gap-3">
                  <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-amber-500/10 text-amber-600 dark:text-amber-400">
                    <Icon name="pill" size={19} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm font-semibold">{product.name}</p>
                    <p className="mt-1 truncate text-xs text-muted-foreground">{product.category}</p>
                  </div>
                  <span className="text-sm font-bold text-amber-600 dark:text-amber-400">{product.quantity}</span>
                </div>
                <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-muted">
                  <div className="h-full rounded-full bg-amber-500" style={{ width: `${Math.max((product.quantity / product.limit) * 100, 8)}%` }} />
                </div>
                <p className="mt-2 text-[11px] text-muted-foreground">الحد الأدنى: {product.limit} وحدة</p>
              </div>
            ))}
          </div>
        </section>
      </div>

      <section className="rounded-2xl border border-border bg-gradient-to-l from-primary/10 via-card to-card p-6 shadow-sm">
        <div className="flex flex-col justify-between gap-5 md:flex-row md:items-center">
          <div>
            <p className="text-sm font-medium text-primary">إجراءات سريعة</p>
            <h2 className="mt-1 text-xl font-bold">إيه اللي محتاج تعمله النهارده؟</h2>
            <p className="mt-2 text-sm text-muted-foreground">اختصارات لأكثر المهام استخدامًا في إدارة الصيدلية.</p>
          </div>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            {[
              { label: 'إضافة موظف', href: '/employees/new', icon: 'users' as const },
              { label: 'استلام مخزون', href: '/inventory', icon: 'package' as const },
              { label: 'تسجيل حضور', href: '/attendance', icon: 'calendar' as const },
              { label: 'تقرير المبيعات', href: '/reports', icon: 'chart' as const },
            ].map((action) => (
              <Link key={action.label} href={action.href} className="flex min-w-[115px] flex-col items-center gap-2 rounded-xl border border-border bg-card/80 px-3 py-4 text-center transition-all hover:-translate-y-0.5 hover:border-primary/30 hover:bg-card">
                <Icon name={action.icon} size={21} className="text-primary" />
                <span className="text-xs font-medium">{action.label}</span>
              </Link>
            ))}
          </div>
        </div>
      </section>
    </div>
  )
}
