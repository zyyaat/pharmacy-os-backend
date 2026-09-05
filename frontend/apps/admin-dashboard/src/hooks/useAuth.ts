// Auth Hook - Real Implementation
import { useState, useEffect, useCallback } from 'react'
import { authApi } from '@/lib/api'
import type { CompanyUser, LoginCredentials, AuthResponse } from '@/types'

interface UseAuthReturn {
  user: CompanyUser | null
  loading: boolean
  error: string | null
  login: (credentials: LoginCredentials) => Promise<AuthResponse>
  logout: () => Promise<void>
  refetch: () => Promise<void>
}

export function useAuth(): UseAuthReturn {
  const [user, setUser] = useState<CompanyUser | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const fetchUser = useCallback(async () => {
    try {
      setLoading(true)
      setError(null)
      
      const profile = await authApi.getProfile()
      setUser(profile)
    } catch (err) {
      // No authenticated user - this is okay
      setUser(null)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    fetchUser()
  }, [fetchUser])

  const login = async (credentials: LoginCredentials): Promise<AuthResponse> => {
    try {
      setLoading(true)
      setError(null)

      const response = await authApi.login(credentials.email, credentials.password)
      setUser(response.user)
      return response
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Login failed'
      setError(message)
      throw err
    } finally {
      setLoading(false)
    }
  }

  const logout = async () => {
    try {
      await authApi.logout()
    } catch (err) {
      console.error('Logout error:', err)
    } finally {
      setUser(null)
    }
  }

  return {
    user,
    loading,
    error,
    login,
    logout,
    refetch: fetchUser,
  }
}
