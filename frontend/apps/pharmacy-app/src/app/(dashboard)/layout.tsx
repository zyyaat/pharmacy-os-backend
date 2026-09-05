'use client'

import { useState } from 'react'
import type { ReactNode } from 'react'
import Header from '@/components/layout/header'
import Sidebar from '@/components/layout/sidebar'

export default function DashboardLayout({ children }: { children: ReactNode }) {
  const [sidebarOpen, setSidebarOpen] = useState(false)

  return (
    <div className="flex min-h-screen overflow-hidden bg-background" dir="rtl">
      <Sidebar mobileOpen={sidebarOpen} onMobileClose={() => setSidebarOpen(false)} />
      <div className="flex min-h-screen min-w-0 flex-1 flex-col">
        <Header onMenuClick={() => setSidebarOpen(true)} />
        <main className="flex-1 overflow-y-auto p-4 lg:p-7">{children}</main>
      </div>
    </div>
  )
}
