// Auth Hook - Real Implementation
import { useState, useEffect, useCallback } from 'react'
import { supabase, isSupabaseConfigured } from '@/lib/supabase'
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
      
      // Try Supabase auth first (if configured)
      if (isSupabaseConfigured()) {
        const { data: { session } } = await supabase.auth.getSession()
        if (session?.user) {
          const userData: CompanyUser = {
            id: session.user.id,
            email: session.user.email || '',
            displayName: session.user.user_metadata?.display_name || '',
            role: session.user.user_metadata?.role || 'viewer',
            isActive: true,
            createdAt: session.user.created_at,
          }
          setUser(userData)
          localStorage.setItem('auth_token', session.access_token)
          return
        }
      }
      
      // Fallback to backend API
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

      // Try Supabase auth first
      if (isSupabaseConfigured()) {
        const { data, error: supabaseError } = await supabase.auth.signInWithPassword({
          email: credentials.email,
          password: credentials.password,
        })

        if (supabaseError) throw new Error(supabaseError.message)

        if (data.user && data.session) {
          const userData: CompanyUser = {
            id: data.user.id,
            email: data.user.email || '',
            displayName: data.user.user_metadata?.display_name || '',
            role: data.user.user_metadata?.role || 'viewer',
            isActive: true,
            createdAt: data.user.created_at,
          }
          
          setUser(userData)
          localStorage.setItem('auth_token', data.session.access_token)
          
          return {
            user: userData,
            token: data.session.access_token,
            expiresIn: data.session.expires_in,
          }
        }
      }

      // Fallback to backend API
      const response = await authApi.login(credentials.email, credentials.password)
      setUser(response.user)
      localStorage.setItem('auth_token', response.token)
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
      if (isSupabaseConfigured()) {
        await supabase.auth.signOut()
      }
      await authApi.logout()
    } catch (err) {
      console.error('Logout error:', err)
    } finally {
      setUser(null)
      localStorage.removeItem('auth_token')
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
