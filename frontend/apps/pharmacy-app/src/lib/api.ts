const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080/api/v1'

function csrfHeaders(): HeadersInit {
  if (typeof document === 'undefined') return {}
  const csrf = document.cookie.match(/(?:^|; )pharmacy_csrf=([^;]+)/)?.[1]
  return csrf ? { 'X-CSRF-Token': decodeURIComponent(csrf) } : {}
}

export async function apiFetch<T>(endpoint: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(`${API_BASE_URL}${endpoint}`, {
    ...options,
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(options.method && options.method !== 'GET' ? csrfHeaders() : {}),
      ...(options.headers || {}),
    },
  })
  const body = await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(body.message || 'API request failed')
  return body as T
}

export const authApi = {
  login(email: string, password: string) {
    return apiFetch<{ user: Record<string, unknown>; expires_in: number }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password, account_type: 'employee' }),
    })
  },
  logout() {
    return apiFetch('/auth/logout', { method: 'POST' })
  },
}
