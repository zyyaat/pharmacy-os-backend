'use client'

import Link from 'next/link'
import { usePathname } from 'next/navigation'
import { cn } from '@/lib/utils'
import { Icon, type IconName } from '@/components/ui'

type NavigationItem = {
  label: string
  href: string
  icon: IconName
  badge?: string
}

const navigation: NavigationItem[] = [
  { label: 'لوحة التحكم', href: '/', icon: 'dashboard' },
  { label: 'المخزون والأدوية', href: '/inventory', icon: 'package', badge: '27' },
  { label: 'الموظفون', href: '/employees', icon: 'users' },
  { label: 'الحضور والانصراف', href: '/attendance', icon: 'calendar' },
  { label: 'الفروع', href: '/branches', icon: 'store' },
  { label: 'التقارير', href: '/reports', icon: 'chart' },
]

export default function Sidebar({
  open,
  onClose,
}: {
  open: boolean
  onClose: () => void
}) {
  const pathname = usePathname()

  const content = (
    <div className="flex h-full flex-col border-l border-border bg-card">
      <div className="flex h-20 items-center gap-3 border-b border-border px-5">
        <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-primary text-lg font-black text-primary-foreground shadow-lg shadow-primary/20">
          P
        </div>
        <div className="min-w-0">
          <p className="truncate text-base font-bold tracking-tight">Pharmacy OS</p>
          <p className="truncate text-xs text-muted-foreground">إدارة الصيدلية</p>
        </div>
        <button
          aria-label="إغلاق القائمة"
          className="mr-auto rounded-lg p-2 text-muted-foreground hover:bg-accent lg:hidden"
          onClick={onClose}
        >
          <Icon name="x" size={18} />
        </button>
      </div>

      <div className="border-b border-border px-4 py-4">
        <div className="rounded-xl bg-primary/8 p-3">
          <p className="text-[11px] font-medium text-muted-foreground">الصيدلية الحالية</p>
          <div className="mt-1 flex items-center justify-between gap-2">
            <p className="truncate text-sm font-semibold">صيدليات الأمل</p>
            <span className="h-2 w-2 rounded-full bg-emerald-500" />
          </div>
          <p className="mt-1 truncate text-xs text-muted-foreground">الفرع الرئيسي · القاهرة</p>
        </div>
      </div>

      <nav className="flex-1 space-y-1 overflow-y-auto p-4">
        <p className="mb-3 px-3 text-[11px] font-bold uppercase tracking-wider text-muted-foreground">
          القائمة الرئيسية
        </p>
        {navigation.map((item) => {
          const active = item.href === '/' ? pathname === '/' : pathname.startsWith(item.href)
          return (
            <Link
              key={item.href}
              href={item.href}
              onClick={onClose}
              className={cn(
                'group relative flex items-center gap-3 rounded-xl px-3 py-3 text-sm font-medium transition-all',
                active
                  ? 'bg-primary/10 text-primary'
                  : 'text-muted-foreground hover:bg-accent hover:text-foreground',
              )}
            >
              {active && <span className="absolute right-0 h-7 w-1 rounded-l-full bg-primary" />}
              <Icon name={item.icon} size={19} className={cn(active && 'text-primary')} />
              <span className="flex-1">{item.label}</span>
              {item.badge && (
                <span className={cn('rounded-full px-2 py-0.5 text-[10px]', active ? 'bg-primary/15 text-primary' : 'bg-muted text-muted-foreground')}>
                  {item.badge}
                </span>
              )}
            </Link>
          )
        })}

        <div className="my-5 border-t border-border" />
        <Link
          href="/settings"
          onClick={onClose}
          className={cn(
            'flex items-center gap-3 rounded-xl px-3 py-3 text-sm font-medium transition-all',
            pathname.startsWith('/settings')
              ? 'bg-primary/10 text-primary'
              : 'text-muted-foreground hover:bg-accent hover:text-foreground',
          )}
        >
          <Icon name="settings" size={19} />
          <span>الإعدادات</span>
        </Link>
      </nav>

      <div className="border-t border-border p-4">
        <div className="flex items-center gap-3 rounded-xl bg-muted/50 p-3">
          <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-primary/15 text-sm font-bold text-primary">
            م
          </div>
          <div className="min-w-0 flex-1">
            <p className="truncate text-sm font-semibold">محمد أحمد</p>
            <p className="truncate text-xs text-muted-foreground">مدير الصيدلية</p>
          </div>
          <button aria-label="تسجيل الخروج" className="text-muted-foreground transition-colors hover:text-destructive">
            <Icon name="logout" size={17} />
          </button>
        </div>
      </div>
    </div>
  )

  return (
    <>
      {open && <button aria-label="إغلاق القائمة" className="fixed inset-0 z-40 bg-black/40 lg:hidden" onClick={onClose} />}
      <aside className={cn('fixed inset-y-0 right-0 z-50 w-[280px] transition-transform duration-300 lg:static lg:block lg:w-[280px] lg:shrink-0', open ? 'translate-x-0' : 'translate-x-full lg:translate-x-0')}>
        {content}
      </aside>
    </>
  )
}
