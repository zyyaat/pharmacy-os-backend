import type { Metadata } from 'next'
import './globals.css'

export const metadata: Metadata = {
  title: 'Pharmacy App - Pharmacy OS',
  description: 'Pharmacy management application',
}

export default function RootLayout({
  children,
}: {
  children: React.ReactNode
}) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  )
}
