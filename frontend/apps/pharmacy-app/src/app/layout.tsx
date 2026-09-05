import type { Metadata } from 'next'
import './globals.css'

export const metadata: Metadata = {
  title: 'Pharmacy OS | إدارة الصيدلية',
  description: 'نظام إدارة الصيدلية والمخزون والموظفين',
}

export default function RootLayout({
  children,
}: {
  children: React.ReactNode
}) {
  return (
    <html lang="ar" dir="rtl">
      <body>{children}</body>
    </html>
  )
}
