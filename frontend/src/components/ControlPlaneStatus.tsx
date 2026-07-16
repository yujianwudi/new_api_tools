import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Clock,
  Database,
  RefreshCw,
  Server,
  ShieldCheck,
  XCircle,
} from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import { apiFetch, createAuthHeaders } from '../lib/api'
import {
  fetchNewAPICapabilities,
  type NewAPICapabilities,
  type NewAPICapabilityData,
} from '../lib/controlPlane'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card'
import { cn } from '../lib/utils'
import { ControlPlaneExplorer } from './ControlPlaneExplorer'

interface DependencyCheck {
  name: string
  status: string
  required: boolean
  diagnostic?: boolean
  latency_ms: number
  details?: Record<string, unknown>
}

interface DependencyHealthResponse {
  success: boolean
  status: string
  version: string
  checked_at: string
  checks: DependencyCheck[]
}

const dependencyLabels: Record<string, string> = {
  main_database: '主数据库',
  tool_store: '审计存储',
  log_database: '日志数据库',
  log_freshness: '日志新鲜度',
  newapi: 'NewAPI',
  redis: 'Redis',
}

const capabilityLabels: Array<[keyof NewAPICapabilities, string]> = [
  ['admin_user_manage', '用户管理 API'],
  ['redemption_api', '兑换码 API'],
  ['upstream_request_id', '上游请求 ID'],
  ['subscription_billing', '订阅计费'],
  ['clickhouse_logs', 'ClickHouse 日志'],
  ['hard_delete_safe', '安全硬删除'],
]

function statusVariant(status: string): 'success' | 'warning' | 'destructive' | 'secondary' {
  if (['healthy', 'fresh', 'ready', 'admin_api'].includes(status)) return 'success'
  if (['degraded', 'stale', 'read_only', 'unknown'].includes(status)) return 'warning'
  if (['unhealthy', 'not_ready'].includes(status)) return 'destructive'
  return 'secondary'
}

function statusLabel(status: string): string {
  const labels: Record<string, string> = {
    healthy: '健康',
    degraded: '降级',
    unhealthy: '异常',
    fresh: '新鲜',
    stale: '滞后',
    empty: '暂无日志',
    unknown: '未知',
    disabled: '未启用',
    admin_api: 'Admin API 写入',
    read_only: '只读保护',
  }
  return labels[status] ?? status
}

function DependencyStatusIcon({ status }: { status: string }) {
  const variant = statusVariant(status)
  if (variant === 'success') {
    return <CheckCircle2 className="h-4 w-4 shrink-0 text-emerald-500" aria-hidden="true" />
  }
  if (variant === 'warning') {
    return <AlertTriangle className="h-4 w-4 shrink-0 text-amber-500" aria-hidden="true" />
  }
  if (variant === 'destructive') {
    return <XCircle className="h-4 w-4 shrink-0 text-red-500" aria-hidden="true" />
  }
  return <AlertTriangle className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden="true" />
}

function formatLag(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds)) return '未知'
  if (seconds < 60) return `${Math.max(0, Math.round(seconds))} 秒`
  if (seconds < 3600) return `${Math.round(seconds / 60)} 分钟`
  return `${(seconds / 3600).toFixed(1)} 小时`
}

async function readDependencyHealth(responsePromise: Promise<Response>): Promise<DependencyHealthResponse> {
  const response = await responsePromise
  const body = await response.json() as Partial<DependencyHealthResponse> & {
    message?: string
    error?: { message?: string }
  }
  const isDiagnosticResponse = response.status === 503 && Array.isArray(body.checks)
  if ((!response.ok && !isDiagnosticResponse) || !Array.isArray(body.checks)) {
    throw new Error(body.error?.message || body.message || `HTTP ${response.status}`)
  }
  return body as DependencyHealthResponse
}

export function ControlPlaneStatus() {
  const { token } = useAuth()
  const [dependencies, setDependencies] = useState<DependencyHealthResponse | null>(null)
  const [capabilities, setCapabilities] = useState<NewAPICapabilityData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const apiUrl = import.meta.env.VITE_API_URL || ''

  const loadStatus = useCallback(async (signal?: AbortSignal) => {
    setLoading(true)
    setError(null)
    const headers = createAuthHeaders(token)

    // These diagnostics are independent. Start both requests together so a
    // slow NewAPI probe does not delay dependency health rendering.
    const [dependencyResult, capabilityResult] = await Promise.allSettled([
      readDependencyHealth(apiFetch(`${apiUrl}/api/health/dependencies`, { headers, signal })),
      fetchNewAPICapabilities({ apiUrl, token, signal }),
    ])

    if (signal?.aborted) return

    const failures: string[] = []
    if (dependencyResult.status === 'fulfilled') {
      setDependencies(dependencyResult.value)
    } else {
      setDependencies(null)
      failures.push(`依赖健康：${dependencyResult.reason instanceof Error ? dependencyResult.reason.message : '加载失败'}`)
    }
    if (capabilityResult.status === 'fulfilled') {
      setCapabilities(capabilityResult.value)
    } else {
      setCapabilities(null)
      failures.push(`NewAPI 能力：${capabilityResult.reason instanceof Error ? capabilityResult.reason.message : '加载失败'}`)
    }
    setError(failures.length > 0 ? failures.join('；') : null)
    setLoading(false)
  }, [apiUrl, token])

  useEffect(() => {
    const controller = new AbortController()
    void loadStatus(controller.signal)
    return () => controller.abort()
  }, [loadStatus])

  const logFreshness = useMemo(
    () => dependencies?.checks.find((check) => check.name === 'log_freshness'),
    [dependencies],
  )
  const lagSeconds = typeof logFreshness?.details?.lag_seconds === 'number'
    ? logFreshness.details.lag_seconds
    : undefined
  const version = capabilities?.status.version?.trim() || '未知'
  const writeMode = capabilities?.write_mode || 'unknown'

  return (
    <div className="space-y-6 animate-in fade-in duration-500">
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <ShieldCheck className="h-7 w-7 text-primary" />
            <h2 className="text-3xl font-bold tracking-tight">控制平面状态</h2>
          </div>
          <p className="text-muted-foreground mt-1">查看旁路控制台依赖、NewAPI 契约版本与写入保护模式。</p>
        </div>
        <Button variant="outline" onClick={() => void loadStatus()} disabled={loading}>
          <RefreshCw className={cn('h-4 w-4 mr-2', loading && 'animate-spin')} />
          刷新状态
        </Button>
      </div>

      {error ? (
        <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-4 text-sm text-destructive">
          <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      ) : null}

      <div className="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4">
        <StatusSummaryCard
          title="控制台健康"
          value={loading && !dependencies ? '检查中' : statusLabel(dependencies?.status || 'unknown')}
          status={dependencies?.status || 'unknown'}
          icon={Activity}
          detail={dependencies?.checked_at ? new Date(dependencies.checked_at).toLocaleString('zh-CN') : '尚未检查'}
        />
        <StatusSummaryCard
          title="NewAPI 版本"
          value={version}
          status={capabilities?.capabilities.known ? 'healthy' : 'unknown'}
          icon={Server}
          detail={capabilities?.status.system_name || '未取得上游状态'}
        />
        <StatusSummaryCard
          title="写入模式"
          value={statusLabel(writeMode)}
          status={writeMode}
          icon={ShieldCheck}
          detail={capabilities?.admin_credentials_configured ? '管理员凭据已配置' : '管理员凭据未配置'}
        />
        <StatusSummaryCard
          title="日志新鲜度"
          value={logFreshness ? statusLabel(logFreshness.status) : '未知'}
          status={logFreshness?.status || 'unknown'}
          icon={Clock}
          detail={`滞后 ${formatLag(lagSeconds)}`}
        />
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-lg">
            <Database className="h-5 w-5 text-primary" />
            依赖健康
          </CardTitle>
          <CardDescription>必需依赖异常会影响就绪状态；可选依赖异常会进入降级模式。</CardDescription>
        </CardHeader>
        <CardContent>
          {dependencies?.checks.length ? (
            <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
              {dependencies.checks.map((check) => (
                <div key={check.name} className="rounded-lg border bg-card p-4">
                  <div className="flex items-start justify-between gap-3">
                    <div className="flex items-center gap-2 min-w-0">
                      <DependencyStatusIcon status={check.status} />
                      <div>
                        <p className="font-medium">{dependencyLabels[check.name] || check.name}</p>
                        <p className="text-xs text-muted-foreground mt-0.5">{check.required ? '必需依赖' : check.diagnostic ? '诊断指标' : '可选依赖'} · {check.latency_ms} ms</p>
                      </div>
                    </div>
                    <Badge variant={statusVariant(check.status)}>{statusLabel(check.status)}</Badge>
                  </div>
                  {check.details ? <DependencyDetails details={check.details} /> : null}
                </div>
              ))}
            </div>
          ) : (
            <p className="py-8 text-center text-sm text-muted-foreground">{loading ? '正在并行检查依赖...' : '暂无依赖数据'}</p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-lg">
            <ShieldCheck className="h-5 w-5 text-primary" />
            NewAPI 能力契约
          </CardTitle>
          <CardDescription>未知版本默认只读；写操作仅在已验证版本和管理员凭据同时可用时开放。</CardDescription>
        </CardHeader>
        <CardContent>
          {capabilities ? (
            <div className="space-y-4">
              <div className="flex flex-wrap gap-2">
                <Badge variant={capabilities.capabilities.known ? 'success' : 'warning'}>
                  {capabilities.capabilities.known ? '版本已验证' : '未知版本，只读保护'}
                </Badge>
                <Badge variant={statusVariant(capabilities.write_mode)}>{statusLabel(capabilities.write_mode)}</Badge>
                <Badge variant={capabilities.admin_credentials_configured ? 'success' : 'warning'}>
                  {capabilities.admin_credentials_configured ? '管理员凭据可用' : '管理员凭据缺失'}
                </Badge>
              </div>
              <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
                {capabilityLabels.map(([key, label]) => {
                  const enabled = capabilities.capabilities[key] === true
                  return (
                    <div key={key} className="flex items-center justify-between rounded-md border px-3 py-2 text-sm">
                      <span>{label}</span>
                      <Badge variant={enabled ? 'success' : 'secondary'}>{enabled ? '支持' : '关闭'}</Badge>
                    </div>
                  )
                })}
              </div>
              <p className="text-xs text-muted-foreground">
                最低已知版本：{capabilities.capabilities.minimum_known_release} · 安全硬删除：{capabilities.capabilities.hard_delete_minimum_known_safe}
              </p>
            </div>
          ) : (
            <p className="py-8 text-center text-sm text-muted-foreground">{loading ? '正在探测 NewAPI 能力...' : '暂无能力数据'}</p>
          )}
        </CardContent>
      </Card>

      <ControlPlaneExplorer token={token} apiUrl={apiUrl} />
    </div>
  )
}

function StatusSummaryCard({
  title,
  value,
  status,
  detail,
  icon: Icon,
}: {
  title: string
  value: string
  status: string
  detail: string
  icon: typeof Activity
}) {
  return (
    <Card>
      <CardContent className="p-5">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <p className="text-sm text-muted-foreground">{title}</p>
            <p className="mt-1 text-xl font-semibold truncate" title={value}>{value}</p>
          </div>
          <div className="rounded-lg bg-primary/10 p-2 text-primary"><Icon className="h-5 w-5" /></div>
        </div>
        <div className="mt-4 flex items-center justify-between gap-2">
          <Badge variant={statusVariant(status)}>{statusLabel(status)}</Badge>
          <span className="text-xs text-muted-foreground truncate" title={detail}>{detail}</span>
        </div>
      </CardContent>
    </Card>
  )
}

function DependencyDetails({ details }: { details: Record<string, unknown> }) {
  const visibleEntries = Object.entries(details).filter(([, value]) => value !== null && value !== '')
  if (visibleEntries.length === 0) return null
  return (
    <dl className="mt-3 space-y-1 border-t pt-3 text-xs">
      {visibleEntries.map(([key, value]) => (
        <div key={key} className="flex items-start justify-between gap-3">
          <dt className="text-muted-foreground">{key.split('_').join(' ')}</dt>
          <dd className="max-w-[65%] break-words text-right font-mono">{String(value)}</dd>
        </div>
      ))}
    </dl>
  )
}
