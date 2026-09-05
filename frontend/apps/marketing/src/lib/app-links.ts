export function getConfiguredPharmacyAppUrl(): string | null {
  const configured = process.env.NEXT_PUBLIC_PHARMACY_APP_URL
  return configured ? configured.replace(/\/$/, '') : null
}

export function getPharmacyAppUrl(): string {
  const configured = getConfiguredPharmacyAppUrl()
  if (configured) return configured

  if (typeof window !== 'undefined') {
    const current = new URL(window.location.href)
    const pharmacyPort = {
      '5002': '5001',
      '3001': '3000',
    }[current.port]
    if (pharmacyPort) {
      current.port = pharmacyPort
      current.pathname = '/'
      current.search = ''
      current.hash = ''
      return current.origin
    }
  }

  return '/pharmacy'
}