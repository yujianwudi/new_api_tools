import { createContext, useContext, useState, useCallback, ReactNode, useEffect } from 'react'
import { setGlobalLogout, clearGlobalLogout } from '../lib/api'

const TOKEN_KEY = 'newapi_tools_token'
const TOKEN_EXPIRY_KEY = 'newapi_tools_token_expiry'

function removeAuthKeys(storage: Storage): void {
  storage.removeItem(TOKEN_KEY)
  storage.removeItem(TOKEN_EXPIRY_KEY)
}

function clearLegacyLocalAuthStorage(): void {
  try {
    removeAuthKeys(window.localStorage)
  } catch {
    // Storage can be unavailable in hardened/private browser contexts.
  }
}

function clearSessionAuthStorage(): void {
  try {
    removeAuthKeys(window.sessionStorage)
  } catch {
    // The in-memory React state remains the source of truth for this page.
  }
}

function clearAllAuthStorage(): void {
  clearLegacyLocalAuthStorage()
  clearSessionAuthStorage()
}

function readSessionAuthValue(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key)
  } catch {
    return null
  }
}

function persistSessionAuth(token: string, expiryTime: number): void {
  clearLegacyLocalAuthStorage()
  try {
    window.sessionStorage.setItem(TOKEN_KEY, token)
    window.sessionStorage.setItem(TOKEN_EXPIRY_KEY, expiryTime.toString())
  } catch {
    clearSessionAuthStorage()
    // The periodic expiry check will fail closed when persistence is unavailable.
  }
}

interface AuthContextType {
  isAuthenticated: boolean
  token: string | null
  login: (password: string) => Promise<boolean>
  logout: () => void
}

const AuthContext = createContext<AuthContextType | null>(null)

export function useAuth() {
  const context = useContext(AuthContext)
  if (!context) {
    throw new Error('useAuth must be used within an AuthProvider')
  }
  return context
}

interface AuthProviderProps {
  children: ReactNode
}

export function AuthProvider({ children }: AuthProviderProps) {
  const [token, setToken] = useState<string | null>(() => {
    // v0.2.0 stops accepting long-lived admin bearer tokens from localStorage.
    clearLegacyLocalAuthStorage()
    const savedToken = readSessionAuthValue(TOKEN_KEY)
    const expiry = readSessionAuthValue(TOKEN_EXPIRY_KEY)

    if (savedToken && expiry) {
      const expiryTime = Number(expiry)
      if (Number.isFinite(expiryTime) && Date.now() < expiryTime) {
        return savedToken
      }
    }
    clearSessionAuthStorage()
    return null
  })

  const isAuthenticated = token !== null

  const logout = useCallback(() => {
    setToken(null)
    clearAllAuthStorage()
  }, [])

  // Check token expiry periodically
  useEffect(() => {
    if (!token) return

    const checkExpiry = () => {
      const expiry = readSessionAuthValue(TOKEN_EXPIRY_KEY)
      const expiryTime = Number(expiry)
      if (!expiry || !Number.isFinite(expiryTime) || Date.now() >= expiryTime) {
        logout()
      }
    }

    const interval = setInterval(checkExpiry, 60000) // Check every minute
    return () => clearInterval(interval)
  }, [logout, token])

  const login = useCallback(async (password: string): Promise<boolean> => {
    const apiUrl = import.meta.env.VITE_API_URL || ''

    let response: Response
    try {
      response = await fetch(`${apiUrl}/api/auth/login`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({ password }),
      })
    } catch (error) {
      // 网络层失败（断网、DNS、连接被拒等）→ 视为服务不可用，而非密码错误
      console.error('Login request failed:', error)
      throw new Error('service_unavailable')
    }

    // 401：密码确实错误（后端 auth.go 对错误密码返回 401）
    if (response.status === 401) {
      return false
    }

    // 其它非 2xx（502/503/504 后端不可达、500 内部错误等）→ 服务不可用，
    // 不能笼统报成“密码错误”误导用户反复试密码
    if (!response.ok) {
      throw new Error('service_unavailable')
    }

    const data = await response.json()
    if (data.success && data.token) {
      const newToken = data.token
      // Parse expires_at or default 24 hours
      let expiryTime: number
      if (data.expires_at) {
        const parsedExpiry = new Date(data.expires_at).getTime()
        expiryTime = Number.isFinite(parsedExpiry) ? parsedExpiry : Date.now() + 86400 * 1000
      } else {
        expiryTime = Date.now() + 86400 * 1000
      }

      persistSessionAuth(newToken, expiryTime)
      setToken(newToken)
      return true
    }
    // 2xx 但 success=false：按密码/凭证错误处理
    return false
  }, [])

  // Set global logout function for API interceptor
  useEffect(() => {
    setGlobalLogout(logout)
    return () => clearGlobalLogout()
  }, [logout])

  return (
    <AuthContext.Provider value={{ isAuthenticated, token, login, logout }}>
      {children}
    </AuthContext.Provider>
  )
}
