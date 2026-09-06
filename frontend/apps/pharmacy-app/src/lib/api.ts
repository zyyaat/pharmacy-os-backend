const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || '/api/v1'

function csrfHeaders(): HeadersInit {
  if (typeof document === 'undefined') return {}
  const csrf = document.cookie.match(/(?:^|; )pharmacy_csrf=([^;]+)/)?.[1]
  return csrf ? { 'X-CSRF-Token': decodeURIComponent(csrf) } : {}
}

let refreshPromise: Promise<boolean> | null = null

export class ApiError extends Error {
  code: string
  status: number

  constructor(message: string, code: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

async function refreshSession(): Promise<boolean> {
  if (!refreshPromise) {
    refreshPromise = fetch(`${API_BASE_URL}/auth/refresh`, {
      method: 'POST',
      credentials: 'include',
      headers: csrfHeaders(),
    })
      .then((response) => response.ok)
      .catch(() => false)
      .finally(() => {
        refreshPromise = null
      })
  }
  return refreshPromise
}

export async function apiFetch<T>(endpoint: string, options: RequestInit = {}, canRefresh = true): Promise<T> {
  const response = await fetch(`${API_BASE_URL}${endpoint}`, {
    ...options,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(options.method && options.method !== 'GET' ? csrfHeaders() : {}),
      ...(options.headers || {}),
    },
  })
  if (response.status === 401 && canRefresh && !endpoint.startsWith('/auth/')) {
    if (await refreshSession()) return apiFetch<T>(endpoint, options, false)
  }
  const body = await response.json().catch(() => ({}))
  if (!response.ok) {
    throw new ApiError(
      body.message || 'API request failed',
      body.code || body.error || 'API_ERROR',
      response.status,
    )
  }
  return body as T
}

export const authApi = {
  login(email: string, password: string) {
    return apiFetch<{ user: Record<string, unknown>; expires_in: number }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    })
  },
  resendVerification(email: string) {
    return apiFetch<{ message: string }>('/auth/resend-verification', {
      method: 'POST',
      body: JSON.stringify({ email }),
    })
  },
  verifyEmail(email: string, code: string) {
    return apiFetch<{ message: string }>('/auth/verify-email', {
      method: 'POST',
      body: JSON.stringify({ email, code }),
    })
  },
  me() {
    return apiFetch<{ user: Record<string, unknown> }>('/auth/me')
  },
  logout() {
    return apiFetch('/auth/logout', { method: 'POST' })
  },
}

export interface PharmacyDashboardStats {
  totalProducts: number
  lowStockCount: number
  activeEmployees: number
  activeToday: number
  salesUnitsToday: number
  lowStockItems: Array<{
    name: string
    generic_name: string
    quantity: number
    min_stock_level: number
    status: string
  }>
}

export const pharmacyApi = {
  getDashboardStats() {
    return apiFetch<PharmacyDashboardStats>('/pharmacy/dashboard/stats')
  },
  getDashboardActivity() {
    return apiFetch<{ data: Array<Record<string, unknown>> }>('/pharmacy/dashboard/activity')
  },
  getInventory() {
    return apiFetch<{ data: Array<Record<string, unknown>> }>('/pharmacy/inventory')
  },
}
