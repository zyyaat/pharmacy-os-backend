// API Client for Backend (Go) - Real Implementation
// This client connects to our Go backend for: CRUD operations, Admin tasks

import type { Company, CompanyUser, DashboardStats, Account } from '@/types'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080/api/v1'

// Generic fetch wrapper with auth
async function apiFetch<T>(
  endpoint: string,
  options: RequestInit = {}
): Promise<T> {
  const url = `${API_BASE_URL}${endpoint}`
  
  // Get auth token from localStorage or cookie
  const token = typeof window !== 'undefined' 
    ? localStorage.getItem('auth_token') || document.cookie?.match(/auth_token=([^;]+)/)?.[1]
    : null

  const headers: HeadersInit = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string>),
  }

  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  try {
    const response = await fetch(url, {
      ...options,
      headers,
    })

    if (!response.ok) {
      const error = await response.json().catch(() => ({ message: 'Request failed' }))
      throw new Error(error.message || `API Error: ${response.status}`)
    }

    return await response.json()
  } catch (error) {
    console.error(`API Error [${endpoint}]:`, error)
    throw error
  }
}

// ============================================
// Auth API
// ============================================

export const authApi = {
  async login(email: string, password: string) {
    return apiFetch<{ user: CompanyUser; token: string; expiresIn: number }>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ email, password }),
    })
  },

  async logout() {
    return apiFetch('/auth/logout', { method: 'POST' })
  },

  async getProfile() {
    return apiFetch<CompanyUser>('/auth/me')
  },

  async changePassword(currentPassword: string, newPassword: string) {
    return apiFetch('/auth/change-password', {
      method: 'POST',
      body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
    })
  },
}

// ============================================
// Companies API
// ============================================

export const companiesApi = {
  async list(params?: { page?: number; limit?: number; search?: string; status?: string }) {
    const queryParams = new URLSearchParams()
    if (params?.page) queryParams.set('page', String(params.page))
    if (params?.limit) queryParams.set('limit', String(params.limit))
    if (params?.search) queryParams.set('search', params.search)
    if (params?.status) queryParams.set('status', params.status)

    const query = queryParams.toString()
    return apiFetch<{ data: Company[]; total: number; page: number; limit: number }>(
      `/companies${query ? `?${query}` : ''}`
    )
  },

  async getById(id: string) {
    return apiFetch<Company>(`/companies/${id}`)
  },

  async create(data: { name: string; name_en?: string; plan: string; max_users?: number }) {
    return apiFetch<Company>('/companies', {
      method: 'POST',
      body: JSON.stringify(data),
    })
  },

  async update(id: string, data: Partial<Company>) {
    return apiFetch<Company>(`/companies/${id}`, {
      method: 'PUT',
      body: JSON.stringify(data),
    })
  },

  async delete(id: string) {
    return apiFetch(`/companies/${id}`, { method: 'DELETE' })
  },
}

// ============================================
// Users API
// ============================================

export const usersApi = {
  async list(companyId: string, params?: { page?: number; limit?: number; role?: string }) {
    const queryParams = new URLSearchParams()
    if (params?.page) queryParams.set('page', String(params.page))
    if (params?.limit) queryParams.set('limit', String(params.limit))
    if (params?.role) queryParams.set('role', params.role)

    const query = queryParams.toString()
    return apiFetch<{ data: CompanyUser[]; total: number }>(
      `/companies/${companyId}/users${query ? `?${query}` : ''}`
    )
  },

  async create(companyId: string, data: {
    email: string
    first_name: string
    last_name: string
    role: string
    password?: string
  }) {
    return apiFetch<CompanyUser>(`/companies/${companyId}/users`, {
      method: 'POST',
      body: JSON.stringify(data),
    })
  },

  async update(companyId: string, userId: string, data: Partial<CompanyUser>) {
    return apiFetch<CompanyUser>(`/companies/${companyId}/users/${userId}`, {
      method: 'PUT',
      body: JSON.stringify(data),
    })
  },

  async delete(companyId: string, userId: string) {
    return apiFetch(`/companies/${companyId}/users/${userId}`, { method: 'DELETE' })
  },
}

// ============================================
// Accounts (Pharmacies) API
// ============================================

export const accountsApi = {
  async list(companyId: string, params?: { page?: number; limit?: number }) {
    const queryParams = new URLSearchParams()
    if (params?.page) queryParams.set('page', String(params.page))
    if (params?.limit) queryParams.set('limit', String(params.limit))

    const query = queryParams.toString()
    return apiFetch<{ data: Account[]; total: number }>(
      `/companies/${companyId}/accounts${query ? `?${query}` : ''}`
    )
  },

  async create(companyId: string, data: { name: string; type: string }) {
    return apiFetch<Account>(`/companies/${companyId}/accounts`, {
      method: 'POST',
      body: JSON.stringify(data),
    })
  },
}

// ============================================
// Dashboard API
// ============================================

export const dashboardApi = {
  async getStats() {
    return apiFetch<DashboardStats>('/dashboard/stats')
  },

  async getRecentActivity(limit = 10) {
    return apiFetch<{ data: DashboardStats['activity'] }>(`/dashboard/activity?limit=${limit}`)
  },
}

// ============================================
// Health Check
// ============================================

export async function healthCheck(): Promise<boolean> {
  try {
    const response = await fetch(`${API_BASE_URL.replace('/api/v1', '')}/health`)
    return response.ok
  } catch {
    return false
  }
}

// Export default API object
export const api = {
  auth: authApi,
  companies: companiesApi,
  users: usersApi,
  accounts: accountsApi,
  dashboard: dashboardApi,
  healthCheck,
}
