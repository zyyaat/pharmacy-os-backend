export function getPharmacyAppUrl(): string {
  const configured = process.env.NEXT_PUBLIC_PHARMACY_APP_URL
  if (configured) return configured.replace(/\/$/, '')

  if (typeof window !== 'undefined') {
    const current = new URL(window.location.href)
    if (current.port === '5002') {
      current.port = '5001'
      return current.origin
    }
  }

  return '/pharmacy'
}