import { useState, useEffect, lazy, Suspense } from 'react'
import { Login, Layout, TabType, Dashboard } from './components'
import { useAuth } from './contexts/AuthContext'
import { WarmupScreen } from './components/WarmupScreen'

// 懒加载非首屏 tab — 显著降低初始包体积
const TopUps = lazy(() => import('./components/TopUps').then(m => ({ default: m.TopUps })))
const RedemptionCenter = lazy(() => import('./components/RedemptionCenter').then(m => ({ default: m.RedemptionCenter })))
const Analytics = lazy(() => import('./components/Analytics').then(m => ({ default: m.Analytics })))
const UserManagement = lazy(() => import('./components/UserManagement').then(m => ({ default: m.UserManagement })))
const RealtimeRanking = lazy(() => import('./components/RealtimeRanking').then(m => ({ default: m.RealtimeRanking })))
const IPAnalysis = lazy(() => import('./components/IPAnalysis').then(m => ({ default: m.IPAnalysis })))
const ModelStatusMonitor = lazy(() => import('./components/ModelStatusMonitor').then(m => ({ default: m.ModelStatusMonitor })))
const Tokens = lazy(() => import('./components/Tokens').then(m => ({ default: m.Tokens })))
const ControlPlaneStatus = lazy(() => import('./components/ControlPlaneStatus').then(m => ({ default: m.ControlPlaneStatus })))

// Valid tabs
const validTabs: TabType[] = ['dashboard', 'control-plane', 'topups', 'risk', 'ip-analysis', 'analytics', 'model-status', 'users', 'tokens', 'redemptions']

// 旧路径迁移：generator / history 现合并到 redemptions 内部 tab
const legacyRedirects: Record<string, string> = {
  generator: '/redemptions?view=generator',
  history: '/redemptions?view=history',
}

// Resolve and normalize both current and historical routes. Invalid history
// entries are replaced so browser back/forward cannot leave the URL and UI out
// of sync.
const resolveTabFromLocation = (): TabType => {
  const pathname = window.location.pathname.replace(/^\/+/, '')
  const mainPath = pathname.split('/')[0]

  if (legacyRedirects[mainPath]) {
    window.history.replaceState(null, '', legacyRedirects[mainPath])
    return 'redemptions'
  }

  if (validTabs.includes(mainPath as TabType)) {
    return mainPath as TabType
  }
  // 兼容旧的 hash 路由，自动迁移。旧版也曾生成 #risk-ip。
  const hash = window.location.hash.replace(/^#\/?/, '')
  const normalizedHash = hash.startsWith('risk-') ? `risk/${hash.slice('risk-'.length)}` : hash
  const [hashMain, ...hashSubPath] = normalizedHash.split('/')
  if (legacyRedirects[hashMain]) {
    window.history.replaceState(null, '', legacyRedirects[hashMain])
    return 'redemptions'
  }
  if (validTabs.includes(hashMain as TabType)) {
    const subPath = hashSubPath.join('/')
    const newPath = subPath ? `/${hashMain}/${subPath}` : `/${hashMain}`
    window.history.replaceState(null, '', newPath)
    return hashMain as TabType
  }

  window.history.replaceState(null, '', '/dashboard')
  return 'dashboard'
}

function App() {
  const { isAuthenticated, token, login, logout } = useAuth()
  const [activeTab, setActiveTab] = useState<TabType>(resolveTabFromLocation)
  const [warmupState, setWarmupState] = useState<'checking' | 'warming' | 'ready'>('checking')

  const apiUrl = import.meta.env.VITE_API_URL || ''

  // 检查后端预热状态
  useEffect(() => {
    if (!isAuthenticated || !token) return

    const checkWarmupStatus = async () => {
      try {
        const response = await fetch(`${apiUrl}/api/system/warmup-status`, {
          headers: {
            'Content-Type': 'application/json',
            'Authorization': `Bearer ${token}`,
          },
        })

        // 处理 401 未授权错误 - token 失效，需要重新登录
        if (response.status === 401) {
          console.warn('Token invalid or expired, logging out...')
          logout()
          return
        }

        // Warmup reporting is optional. A truthful 501 means the backend does
        // not expose a warmup state, so do not trap the UI on synthetic
        // progress.
        if (response.status === 501) {
          setWarmupState('ready')
          return
        }

        const data = await response.json()

        if (data.success && data.data.status === 'ready') {
          // 后端已预热完成，直接进入
          setWarmupState('ready')
        } else {
          // 后端还在预热中，显示预热界面
          setWarmupState('warming')
        }
      } catch {
        // 网络错误，显示预热界面让它处理
        setWarmupState('warming')
      }
    }

    checkWarmupStatus()
  }, [isAuthenticated, token, apiUrl, logout])

  // Sync tab with URL pathname (History API)
  // Only update if main path segment changes, preserve sub-routes
  useEffect(() => {
    const pathname = window.location.pathname.slice(1)
    const currentMainPath = pathname.split('/')[0]
    if (currentMainPath !== activeTab) {
      window.history.pushState(null, '', `/${activeTab}`)
    }
  }, [activeTab])

  // Listen for popstate (browser back/forward)
  useEffect(() => {
    const handlePopState = () => {
      setActiveTab(resolveTabFromLocation())
    }
    window.addEventListener('popstate', handlePopState)
    return () => window.removeEventListener('popstate', handlePopState)
  }, [])

  const handleWarmupReady = () => {
    setWarmupState('ready')
  }

  if (!isAuthenticated) {
    return <Login onLogin={login} />
  }

  // 正在检查预热状态
  if (warmupState === 'checking') {
    return (
      <div className="min-h-screen flex items-center justify-center bg-background">
        <div className="flex flex-col items-center gap-4">
          <div className="relative">
            <div className="w-12 h-12 rounded-full border-4 border-primary/20 border-t-primary animate-spin" />
          </div>
          <p className="text-sm text-muted-foreground animate-pulse">正在连接服务器...</p>
        </div>
      </div>
    )
  }

  // 显示预热界面（后端还在预热中）
  if (warmupState === 'warming') {
    return <WarmupScreen onReady={handleWarmupReady} />
  }

  const renderContent = () => {
    switch (activeTab) {
      case 'dashboard':
        return <Dashboard />
      case 'control-plane':
        return <ControlPlaneStatus />
      case 'redemptions':
        return <RedemptionCenter />
      case 'topups':
        return <TopUps />
      case 'risk':
        return <RealtimeRanking />
      case 'ip-analysis':
        return <IPAnalysis />
      case 'analytics':
        return <Analytics />
      case 'model-status':
        return <ModelStatusMonitor />
      case 'users':
        return <UserManagement />
      case 'tokens':
        return <Tokens />
      default:
        return <Dashboard />
    }
  }

  return (
    <Layout activeTab={activeTab} onTabChange={setActiveTab} onLogout={logout}>
      <Suspense
        fallback={
          <div className="min-h-[400px] flex items-center justify-center">
            <div className="w-8 h-8 rounded-full border-4 border-primary/20 border-t-primary animate-spin" />
          </div>
        }
      >
        {renderContent()}
      </Suspense>
    </Layout>
  )
}

export default App
