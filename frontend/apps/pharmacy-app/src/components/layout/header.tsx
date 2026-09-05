'use client'

import { useEffect, useState } from 'react'
import { Icon } from '@/components/ui'

export default function Header({ onMenuClick }: { onMenuClick: () => void }) {
  const [dark, setDark] = useState(false)

  useEffect(() => {
    const saved = window.localStorage.getItem('pharmacy-theme')
    const isDark = saved === 'dark'
    setDark(isDark)
    document.documentElement.classList.toggle('dark', isDark)
  }, [])

  const toggleTheme = () => {
    const next = !dark
    setDark(next)
    document.documentElement.classList.toggle('dark', next)
    window.localStorage.setItem('pharmacy-theme', next ? 'dark' : 'light')
  }

  return (
    <header className="sticky top-0 z-30 flex h-20 items-center gap-4 border-b border-border bg-background/85 px-4 backdrop-blur-md lg:px-7">
      <button
        aria-label="فتح القائمة"
        className="rounded-xl p-2 text-muted-foreground hover:bg-accent lg:hidden"
        onClick={onMenuClick}
      >
        <Icon name="menu" size={22} />
      </button>

      <div className="relative hidden max-w-md flex-1 md:block">
        <Icon name="search" size={18} className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground" />
        <input
          aria-label="بحث"
          className="h-11 w-full rounded-xl border border-input bg-card pr-10 pl-4 text-sm outline-none transition-all placeholder:text-muted-foreground focus:border-primary focus:ring-4 focus:ring-primary/10"
          placeholder="ابحث عن دواء، موظف، أو عملية..."
          type="search"
        />
      </div>

      <div className="mr-auto flex items-center gap-1 sm:gap-2">
        <button
          aria-label="تغيير المظهر"
          className="rounded-xl p-2.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          onClick={toggleTheme}
        >
          <Icon name={dark ? 'sun' : 'moon'} size={19} />
        </button>
        <button
          aria-label="الإشعارات"
          className="relative rounded-xl p-2.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <Icon name="bell" size={19} />
          <span className="absolute right-1 top-1 h-4 min-w-4 rounded-full bg-destructive px-1 text-center text-[10px] font-bold leading-4 text-destructive-foreground">
            3
          </span>
        </button>
        <div className="hidden h-8 w-px bg-border sm:block" />
        <div className="hidden items-center gap-3 sm:flex">
          <div className="text-left">
            <p className="text-sm font-semibold">صباح الخير، محمد</p>
            <p className="text-xs text-muted-foreground">الأربعاء، 12 يونيو 2024</p>
          </div>
          <div className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/15 font-bold text-primary">
            م
          </div>
        </div>
      </div>
    </header>
  )
}
