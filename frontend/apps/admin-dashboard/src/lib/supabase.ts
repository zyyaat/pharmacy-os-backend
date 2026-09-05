// Supabase Admin Client - Real Implementation
// This client connects to Supabase for: Auth, Realtime, Storage

import { createClient } from '@supabase/supabase-js'

const supabaseUrl = process.env.NEXT_PUBLIC_SUPABASE_URL || ''
const supabaseAnonKey = process.env.NEXT_PUBLIC_SUPABASE_ANON_KEY || ''

if (!supabaseUrl || !supabaseAnonKey) {
  console.warn('⚠️ Supabase credentials not configured. Please set NEXT_PUBLIC_SUPABASE_URL and NEXT_PUBLIC_SUPABASE_ANON_KEY in .env.local')
}

export const supabase = createClient(supabaseUrl, supabaseAnonKey, {
  auth: {
    autoRefreshToken: true,
    persistSession: true,
    detectSessionInUrl: true,
  },
  realtime: {
    params: {
      eventsPerSecond: 10,
    },
  },
})

// Type helpers for Supabase queries
export type Database = {
  public: {
    Tables: {
      companies: {
        Row: {
          id: string
          name: string
          name_en?: string
          status: 'active' | 'suspended' | 'trial' | 'cancelled'
          plan: 'free' | 'starter' | 'professional' | 'enterprise' | 'custom'
          logo_url?: string
          max_users: number
          created_at: string
          updated_at: string
        }
        Insert: {
          id?: string
          name: string
          name_en?: string
          status?: 'active' | 'suspended' | 'trial' | 'cancelled'
          plan?: 'free' | 'starter' | 'professional' | 'enterprise' | 'custom'
          logo_url?: string
          max_users?: number
        }
        Update: {
          id?: string
          name?: string
          name_en?: string
          status?: 'active' | 'suspended' | 'trial' | 'cancelled'
          plan?: 'free' | 'starter' | 'professional' | 'enterprise' | 'custom'
          logo_url?: string
          max_users?: number
        }
      }
      company_users: {
        Row: {
          id: string
          company_id: string
          email: string
          first_name: string
          last_name: string
          display_name?: string
          avatar_url?: string
          role: 'super_admin' | 'company_admin' | 'company_manager' | 'viewer'
          is_active: boolean
          last_login_at?: string
          created_at: string
          updated_at: string
        }
      }
      accounts: {
        Row: {
          id: string
          company_id: string
          name: string
          type: 'pharmacy' | 'chain' | 'hospital'
          is_active: boolean
          created_at: string
        }
      }
    }
  }
}

// Helper function to check if Supabase is configured
export function isSupabaseConfigured(): boolean {
  return !!(supabaseUrl && supabaseAnonKey && 
    supabaseUrl !== 'YOUR_SUPABASE_URL_HERE' && 
    supabaseAnonKey !== 'YOUR_SUPABASE_ANON_KEY_HERE')
}
