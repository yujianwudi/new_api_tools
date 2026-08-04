import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import {
  Activity, AlertTriangle, CheckCircle2, ChevronRight, Clock3, Filter,
  Gauge, Loader2, Play, RefreshCw, Search, ServerCog, Settings2, ShieldAlert,
  TimerReset, Zap,
} from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import { apiFetch, createAuthHeaders } from '../lib/api'
import { chunkModelNames, mapWithConcurrency, MODEL_STATUS_BATCH_MAX_CONCURRENCY } from '../lib/modelStatusBatch'
import { cn } from '../lib/utils'
import { useToast } from './Toast'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Card, CardContent } from './ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from './ui/dialog'
import {
  authoritativeTrafficSourceState,
  combineFreshSourceHealth,
  settleStatusSource,
  shouldCommitStatusSnapshot,
  type CombinedHealth,
  type SourceSnapshot,
} from './modelStatusTruth'

type TrafficHealth = 'healthy' | 'degraded' | 'unhealthy' | 'unknown'
type SourceState = 'fresh' | 'stale' | 'empty' | 'unavailable' | 'pending' | 'unsupported'
type ProbeHealth = 'healthy' | 'degraded' | 'unhealthy' | 'stale' | 'unavailable' | 'pending' | 'unsupported'

interface AvailableModel {
  model_name: string
  request_count_24h: number
}

interface SlotStatus {
  slot: number
  start_time: number
  end_time: number
  total_requests: number
  success_count: number
  failure_count?: number
  success_rate: number
  status: 'green' | 'yellow' | 'red' | 'unknown'
}

interface TrafficStatus {
  model_name: string
  display_name: string
  time_window: string
  total_requests: number
  success_count: number
  failure_count?: number
  success_rate: number | null
  current_status: 'green' | 'yellow' | 'red' | 'unknown'
  traffic_health?: TrafficHealth
  source_state?: SourceState
  last_traffic_at?: number
  fetched_at?: string
  slot_data: SlotStatus[]
}

interface ProbeAttempt {
  id: number
  run_id: number
  model_name: string
  capability: string
  endpoint: string
  outcome: 'success' | 'failure' | 'skipped'
  protocol_success: boolean
  semantic_success: boolean | null
  http_status: number
  header_latency_ms: number | null
  first_token_latency_ms: number | null
  total_latency_ms: number
  error_code?: string
  error_message?: string
  started_at: string
  finished_at: string
}

interface ProbeItem {
  model_name: string
  capability: string
  probe_health: ProbeHealth
  source_state: SourceState
  availability_24h: number | null
  attempt_count_24h: number
  success_count_24h: number
  failure_count_24h: number
  skipped_count_24h: number
  average_header_latency_ms: number | null
  average_first_token_latency_ms: number | null
  average_total_latency_ms: number | null
  latest?: ProbeAttempt
  reason_code: string
  reason: string
  allowed: boolean
}

interface ProbeRun {
  id: number
  status: string
  requested_count: number
  attempted_count: number
  success_count: number
  failure_count: number
  skipped_count: number
  started_at: string
  finished_at?: string
}

interface ProbeConfig {
  state: 'disabled' | 'misconfigured' | 'ready' | 'running' | 'budget_exhausted' | 'unavailable'
  enabled: boolean
  configured: boolean
  running: boolean
  message?: string
  interval_seconds: number
  stale_seconds: number
  max_concurrency: number
  max_models_per_run: number
  daily_request_budget: number
  requests_used_today: number
  requests_remaining_today: number
  retention_days: number
  allowed_models: string[]
  next_run_at?: string
  last_run?: ProbeRun
}

interface ProbeSummary {
  config: ProbeConfig
  items: ProbeItem[]
  fetched_at?: string
}

interface TokenGroup {
  group_name: string
  model_count: number
  models: string[]
}

interface ModelRow {
  modelName: string
  traffic?: TrafficStatus
  probe?: ProbeItem
  combined: CombinedHealth
  reasons: string[]
}

const WINDOW_OPTIONS = [
  { value: '1h', label: '最近 1 小时' },
  { value: '6h', label: '最近 6 小时' },
  { value: '12h', label: '最近 12 小时' },
  { value: '24h', label: '最近 24 小时' },
]

const combinedMeta: Record<CombinedHealth, { label: string; dot: string; badge: string }> = {
  healthy: { label: '双源健康', dot: 'bg-emerald-500', badge: 'border-emerald-200 bg-emerald-50 text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950/40 dark:text-emerald-300' },
  degraded: { label: '需要关注', dot: 'bg-amber-500', badge: 'border-amber-200 bg-amber-50 text-amber-700 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300' },
  unhealthy: { label: '确认异常', dot: 'bg-rose-500', badge: 'border-rose-200 bg-rose-50 text-rose-700 dark:border-rose-900 dark:bg-rose-950/40 dark:text-rose-300' },
  observing: { label: '单源观测', dot: 'bg-sky-500', badge: 'border-sky-200 bg-sky-50 text-sky-700 dark:border-sky-900 dark:bg-sky-950/40 dark:text-sky-300' },
  unknown: { label: '证据不足', dot: 'bg-slate-400', badge: 'border-slate-200 bg-slate-50 text-slate-600 dark:border-slate-800 dark:bg-slate-900 dark:text-slate-300' },
}

const probeStateLabel: Record<ProbeHealth, string> = {
  healthy: '探测健康', degraded: '探测波动', unhealthy: '探测失败', stale: '探测过期',
  unavailable: '未配置', pending: '探测中', unsupported: '未适配',
}

function parseEnvelope<T>(value: unknown): T {
  if (typeof value !== 'object' || value === null || !('success' in value) || value.success !== true || !('data' in value)) {
    throw new Error('服务端返回了无效响应')
  }
  return value.data as T
}

async function readJSON<T>(response: Response): Promise<T> {
  const payload = await response.json().catch(() => null)
  if (!response.ok) {
    const message = typeof payload === 'object' && payload !== null && 'message' in payload && typeof payload.message === 'string'
      ? payload.message
      : `请求失败（HTTP ${response.status}）`
    throw new Error(message)
  }
  return parseEnvelope<T>(payload)
}

function requestErrorMessage(error: unknown, fallback: string) {
  return error instanceof Error && error.message ? error.message : fallback
}

function trafficHealthOf(status?: TrafficStatus): TrafficHealth {
  if (!status) return 'unknown'
  if (status.traffic_health) return status.traffic_health
  return status.current_status === 'green' ? 'healthy'
    : status.current_status === 'yellow' ? 'degraded'
      : status.current_status === 'red' ? 'unhealthy' : 'unknown'
}

function sourceStateOf(status?: TrafficStatus): SourceState {
  if (!status) return 'unavailable'
  return authoritativeTrafficSourceState(status.source_state)
}

function combineHealth(traffic?: TrafficStatus, probe?: ProbeItem): { combined: CombinedHealth; reasons: string[] } {
  const trafficSource = sourceStateOf(traffic)
  const trafficHealth = trafficHealthOf(traffic)
  const probeSource = probe?.source_state ?? 'unavailable'
  const probeHealth = probe?.probe_health ?? 'unavailable'
  const reasons: string[] = []

  if (trafficHealth === 'unhealthy') reasons.push(`真实流量成功率 ${formatPercent(traffic?.success_rate)}`)
  if (trafficHealth === 'degraded') reasons.push(`真实流量成功率下降至 ${formatPercent(traffic?.success_rate)}`)
  if (trafficSource === 'stale') reasons.push('真实流量证据已过期')
  if (trafficSource === 'empty') reasons.push('当前窗口没有真实调用')
  if (trafficSource === 'unavailable') reasons.push('真实流量数据源不可用')
  if (probeHealth === 'unhealthy') reasons.push(probe?.reason || '最近主动探测失败')
  if (probeHealth === 'degraded') reasons.push(probe?.reason || '主动探测可用率低于目标')
  if (probeSource === 'stale') reasons.push('主动探测结果已过期')
  if (probeSource === 'unsupported') reasons.push('模型能力尚未配置安全探测适配器')
  if (probeSource === 'pending') reasons.push('主动探测尚未产生结果')
  if (probeSource === 'unavailable') reasons.push(probe?.reason || '主动探测数据源不可用')

  const combined = combineFreshSourceHealth(trafficHealth, trafficSource, probeHealth, probeSource)
  if (combined === 'healthy') {
    return { combined: 'healthy', reasons: ['真实流量和主动探测均通过'] }
  }
  if (combined === 'observing') return { combined, reasons: reasons.length ? reasons : ['仅有一个数据源提供健康证据'] }
  return { combined, reasons: reasons.length ? reasons : ['没有足够的新鲜证据判断模型状态'] }
}

function formatPercent(value?: number | null) {
  return typeof value === 'number' && Number.isFinite(value) ? `${value.toFixed(value >= 99.9 ? 2 : 1)}%` : '—'
}

function formatLatency(value?: number | null) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—'
  return value >= 1000 ? `${(value / 1000).toFixed(2)}s` : `${Math.round(value)}ms`
}

function formatTime(value?: string | number | null) {
  if (!value) return '—'
  const date = typeof value === 'number' ? new Date(value * 1000) : new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }).format(date)
}

function relativeTime(value?: string | number | null) {
  if (!value) return '无数据'
  const date = typeof value === 'number' ? new Date(value * 1000) : new Date(value)
  if (Number.isNaN(date.getTime())) return '无数据'
  const seconds = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000))
  if (seconds < 60) return `${seconds} 秒前`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`
  return `${Math.floor(seconds / 86400)} 天前`
}

function idempotencyKey() {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return `probe-${crypto.randomUUID()}`
  return `probe-${Date.now()}-${Math.random().toString(16).slice(2)}`
}

export function ModelStatusConsole() {
  const { token } = useAuth()
  const { showToast } = useToast()
  const apiUrl = import.meta.env.VITE_API_URL || ''
  const headers = useMemo(() => createAuthHeaders(token), [token])

  const [availableModels, setAvailableModels] = useState<AvailableModel[]>([])
  const [selectedModels, setSelectedModels] = useState<string[]>([])
  const [maxBatch, setMaxBatch] = useState(200)
  const [tokenGroups, setTokenGroups] = useState<TokenGroup[]>([])
  const [trafficSnapshot, setTrafficSnapshot] = useState<SourceSnapshot<TrafficStatus[]>>({
    data: [], state: 'unavailable', window: '', fetchedAt: null, error: '',
  })
  const [probeSnapshot, setProbeSnapshot] = useState<SourceSnapshot<ProbeSummary | null>>({
    data: null, state: 'unavailable', window: '', fetchedAt: null, error: '',
  })
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [runningProbe, setRunningProbe] = useState(false)
  const [error, setError] = useState('')
  const [timeWindow, setTimeWindow] = useState('24h')
  const activeWindowRef = useRef(timeWindow)
  activeWindowRef.current = timeWindow
  const [search, setSearch] = useState('')
  const deferredSearch = useDeferredValue(search.trim().toLowerCase())
  const [statusFilter, setStatusFilter] = useState<'all' | CombinedHealth>('all')
  const [groupFilter, setGroupFilter] = useState('all')
  const [selectorOpen, setSelectorOpen] = useState(false)
  const [selectorSearch, setSelectorSearch] = useState('')
  const [selectorDraft, setSelectorDraft] = useState<string[]>([])
  const [detailModel, setDetailModel] = useState<string | null>(null)
  const [probeHistory, setProbeHistory] = useState<ProbeAttempt[]>([])
  const [historyLoading, setHistoryLoading] = useState(false)
  const refreshController = useRef<AbortController | null>(null)
  const refreshRequestId = useRef(0)
  const selectorTriggerRef = useRef<HTMLElement | null>(null)
  const detailTriggerRef = useRef<HTMLElement | null>(null)

  const trafficStatuses = useMemo(
    () => trafficSnapshot.state === 'fresh' && trafficSnapshot.window === timeWindow ? trafficSnapshot.data : [],
    [timeWindow, trafficSnapshot],
  )
  const probeSummary = useMemo(
    () => probeSnapshot.state === 'fresh' && probeSnapshot.window === timeWindow ? probeSnapshot.data : null,
    [probeSnapshot, timeWindow],
  )

  const monitorModels = useMemo(() => {
    if (selectedModels.length > 0) return selectedModels
    return availableModels.slice(0, Math.min(20, maxBatch)).map(model => model.model_name)
  }, [availableModels, maxBatch, selectedModels])

  const loadFoundation = useCallback(async (signal: AbortSignal) => {
    const [modelsResponse, configResponse, groupsResponse] = await Promise.all([
      apiFetch(`${apiUrl}/api/model-status/models`, { headers, signal }),
      apiFetch(`${apiUrl}/api/model-status/config/selected`, { headers, signal }),
      apiFetch(`${apiUrl}/api/model-status/token-groups`, { headers, signal }),
    ])
    const [modelsPayload, configPayload, groupsPayload] = await Promise.all([
      modelsResponse.json(), configResponse.json(), groupsResponse.json(),
    ])
    const models = parseEnvelope<AvailableModel[]>(modelsPayload)
    if (typeof configPayload !== 'object' || configPayload === null || configPayload.success !== true) throw new Error('模型监测配置不可用')
    const groups = parseEnvelope<TokenGroup[]>(groupsPayload)
    const configData = 'data' in configPayload && Array.isArray(configPayload.data) ? configPayload.data.filter((item: unknown): item is string => typeof item === 'string') : []
    setAvailableModels(Array.isArray(models) ? models : [])
    setSelectedModels(configData)
    setSelectorDraft(configData)
    setMaxBatch(typeof configPayload.max_batch === 'number' ? Math.max(1, Math.min(200, configPayload.max_batch)) : 200)
    if (typeof configPayload.time_window === 'string' && WINDOW_OPTIONS.some(item => item.value === configPayload.time_window)) {
      setTimeWindow(configPayload.time_window)
    }
    setTokenGroups(Array.isArray(groups) ? groups : [])
  }, [apiUrl, headers])

  const loadStatuses = useCallback(async (models: string[], window: string, signal: AbortSignal) => {
    const requestId = ++refreshRequestId.current
    if (models.length === 0) {
      const fetchedAt = new Date().toISOString()
      setTrafficSnapshot({ data: [], state: 'fresh', window, fetchedAt, error: '' })
      setProbeSnapshot({ data: null, state: 'fresh', window, fetchedAt, error: '' })
      return [] as string[]
    }
    const trafficPromise = mapWithConcurrency(
      chunkModelNames(models, maxBatch),
      MODEL_STATUS_BATCH_MAX_CONCURRENCY,
      async names => {
        const response = await apiFetch(`${apiUrl}/api/model-status/status/batch?window=${window}`, {
          method: 'POST', headers, body: JSON.stringify(names), signal,
        })
        return readJSON<TrafficStatus[]>(response)
      },
    ).then(chunks => chunks.flat())
    const probePromise = apiFetch(`${apiUrl}/api/model-status/probes/summary`, {
      method: 'POST', headers, body: JSON.stringify({ models }), signal,
    }).then(response => readJSON<ProbeSummary>(response))

    const stillCurrent = () => shouldCommitStatusSnapshot(
      refreshRequestId.current, requestId, activeWindowRef.current, window, signal.aborted,
    )
    const trafficSettlement = settleStatusSource({
      promise: trafficPromise,
      unavailableData: [],
      window,
      fallbackError: '真实流量状态刷新失败',
      canCommit: stillCurrent,
      fetchedAt: traffic => traffic.find(item => item.fetched_at)?.fetched_at,
      commit: setTrafficSnapshot,
    })
    const probeSettlement = settleStatusSource({
      promise: probePromise,
      unavailableData: null,
      window,
      fallbackError: '主动探测状态刷新失败',
      canCommit: stillCurrent,
      fetchedAt: probes => probes?.fetched_at,
      commit: setProbeSnapshot,
    })
    const settlements = await Promise.allSettled([trafficSettlement, probeSettlement])
    if (!stillCurrent()) return [] as string[]
    return settlements.flatMap(result => result.status === 'fulfilled' && result.value ? [result.value] : [])
  }, [apiUrl, headers, maxBatch])

  const refresh = useCallback(async (showSpinner = true) => {
    refreshController.current?.abort()
    const controller = new AbortController()
    refreshController.current = controller
    if (showSpinner) setRefreshing(true)
    setError('')
    try {
      const sourceErrors = await loadStatuses(monitorModels, timeWindow, controller.signal)
      if (controller.signal.aborted) return
      if (sourceErrors.length > 0) {
        const message = sourceErrors.join('；')
        setError(message)
        if (showSpinner) showToast('error', message)
      }
    } catch (requestError) {
      if (controller.signal.aborted) return
      const message = requestErrorMessage(requestError, '模型状态刷新失败')
      setError(message)
      if (showSpinner) showToast('error', message)
    } finally {
      if (refreshController.current === controller) {
        refreshController.current = null
        setRefreshing(false)
      }
    }
  }, [loadStatuses, monitorModels, showToast, timeWindow])

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError('')
    void loadFoundation(controller.signal)
      .catch(foundationError => {
        if (!controller.signal.aborted) setError(foundationError instanceof Error ? foundationError.message : '模型监测初始化失败')
      })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [loadFoundation])

  useEffect(() => {
    if (loading) return
    void refresh(false)
    return () => refreshController.current?.abort()
  }, [loading, refresh])

  useEffect(() => {
    if (loading) return
    const interval = window.setInterval(() => { void refresh(false) }, 60_000)
    return () => window.clearInterval(interval)
  }, [loading, refresh])

  useEffect(() => {
    if (!detailModel) {
      setProbeHistory([])
      return
    }
    const controller = new AbortController()
    setHistoryLoading(true)
    void apiFetch(`${apiUrl}/api/model-status/probes/history?model=${encodeURIComponent(detailModel)}&limit=20`, { headers, signal: controller.signal })
      .then(response => readJSON<ProbeAttempt[]>(response))
      .then(setProbeHistory)
      .catch(historyError => {
        if (!controller.signal.aborted) showToast('error', historyError instanceof Error ? historyError.message : '探测历史加载失败')
      })
      .finally(() => { if (!controller.signal.aborted) setHistoryLoading(false) })
    return () => controller.abort()
  }, [apiUrl, detailModel, headers, showToast])

  const rows = useMemo<ModelRow[]>(() => {
    const trafficMap = new Map(trafficStatuses.map(item => [item.model_name, item]))
    const probeMap = new Map((probeSummary?.items ?? []).map(item => [item.model_name, item]))
    return monitorModels.map(modelName => {
      const traffic = trafficMap.get(modelName)
      const probe = probeMap.get(modelName)
      const combined = combineHealth(traffic, probe)
      return { modelName, traffic, probe, combined: combined.combined, reasons: combined.reasons }
    })
  }, [monitorModels, probeSummary, trafficStatuses])

  const visibleRows = useMemo(() => {
    const groupModels = groupFilter === 'all' ? null : new Set(tokenGroups.find(group => group.group_name === groupFilter)?.models ?? [])
    return rows.filter(row => {
      if (deferredSearch && !row.modelName.toLowerCase().includes(deferredSearch)) return false
      if (statusFilter !== 'all' && row.combined !== statusFilter) return false
      if (groupModels && !groupModels.has(row.modelName)) return false
      return true
    }).sort((left, right) => {
      const rank: Record<CombinedHealth, number> = { unhealthy: 0, degraded: 1, unknown: 2, observing: 3, healthy: 4 }
      return rank[left.combined] - rank[right.combined] || left.modelName.localeCompare(right.modelName)
    })
  }, [deferredSearch, groupFilter, rows, statusFilter, tokenGroups])

  const stats = useMemo(() => ({
    total: rows.length,
    attention: rows.filter(row => row.combined === 'unhealthy' || row.combined === 'degraded').length,
    healthy: rows.filter(row => row.combined === 'healthy').length,
    unknown: rows.filter(row => row.combined === 'unknown').length,
  }), [rows])

  const attentionRows = useMemo(() => rows.filter(row => row.combined === 'unhealthy' || row.combined === 'degraded').slice(0, 6), [rows])
  const selectedDetail = useMemo(() => rows.find(row => row.modelName === detailModel), [detailModel, rows])

  const openSelector = useCallback(() => {
    selectorTriggerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectorDraft(selectedModels)
    setSelectorOpen(true)
  }, [selectedModels])

  const openDetail = useCallback((modelName: string) => {
    detailTriggerRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setDetailModel(modelName)
  }, [])

  const restoreSelectorFocus = useCallback((event: Event) => {
    event.preventDefault()
    selectorTriggerRef.current?.focus()
    selectorTriggerRef.current = null
  }, [])

  const restoreDetailFocus = useCallback((event: Event) => {
    event.preventDefault()
    detailTriggerRef.current?.focus()
    detailTriggerRef.current = null
  }, [])

  const saveSelection = useCallback(async () => {
    try {
      const response = await apiFetch(`${apiUrl}/api/model-status/config/selected`, {
        method: 'PUT', headers, body: JSON.stringify({ models: selectorDraft }),
      })
      const saved = await readJSON<string[]>(response)
      setSelectedModels(saved)
      setSelectorDraft(saved)
      setSelectorOpen(false)
      showToast('success', saved.length > 0 ? `已监控 ${saved.length} 个模型` : '已清空固定模型，页面将展示流量最高的模型')
    } catch (saveError) {
      showToast('error', saveError instanceof Error ? saveError.message : '模型选择保存失败')
    }
  }, [apiUrl, headers, selectorDraft, showToast])

  const runProbe = useCallback(async () => {
    const allowed = new Set(probeSummary?.config.allowed_models ?? [])
    const requested = monitorModels.filter(model => allowed.has(model)).slice(0, probeSummary?.config.max_models_per_run ?? 20)
    if (requested.length === 0) {
      showToast('error', '当前展示模型没有进入主动探测白名单')
      return
    }
    setRunningProbe(true)
    try {
      const response = await apiFetch(`${apiUrl}/api/model-status/probes/run`, {
        method: 'POST', headers: { ...headers, 'Idempotency-Key': idempotencyKey() },
        body: JSON.stringify({ models: requested }),
      })
      await readJSON<ProbeRun>(response)
      showToast('success', `已提交 ${requested.length} 个模型的主动探测`)
      window.setTimeout(() => { void refresh(false) }, 1200)
    } catch (probeError) {
      showToast('error', probeError instanceof Error ? probeError.message : '主动探测提交失败')
    } finally {
      setRunningProbe(false)
    }
  }, [apiUrl, headers, monitorModels, probeSummary, refresh, showToast])

  if (loading) {
    return <div className="min-h-[520px] flex items-center justify-center"><Loader2 className="h-8 w-8 animate-spin text-primary" /></div>
  }

  return (
    <div className="space-y-5">
      <section className="overflow-hidden rounded-2xl border border-border/70 bg-card shadow-sm">
        <div className="flex flex-col gap-5 border-b border-border/60 bg-gradient-to-br from-slate-950 via-slate-900 to-slate-800 px-5 py-6 text-white sm:px-7 lg:flex-row lg:items-center lg:justify-between">
          <div className="max-w-3xl">
            <div className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.18em] text-sky-300">
              <Activity className="h-4 w-4" /> Model Reliability Console
            </div>
            <h2 className="text-2xl font-semibold tracking-tight sm:text-3xl">模型可靠性监控</h2>
            <p className="mt-2 max-w-2xl text-sm leading-6 text-slate-300">真实流量回答“用户是否受影响”，主动探测回答“没有流量时模型是否仍可用”。任何数据缺失或过期都不会被显示成绿色。</p>
          </div>
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" onClick={openSelector} className="border-white/20 bg-white/10 text-white hover:bg-white/20 hover:text-white">
              <Settings2 className="mr-2 h-4 w-4" /> 监控范围
            </Button>
            <Button variant="outline" onClick={() => void refresh(true)} disabled={refreshing} className="border-white/20 bg-white/10 text-white hover:bg-white/20 hover:text-white">
              <RefreshCw className={cn('mr-2 h-4 w-4', refreshing && 'animate-spin')} /> 刷新
            </Button>
            <Button onClick={() => void runProbe()} disabled={runningProbe || !probeSummary?.config.configured || probeSummary.config.running || probeSummary.config.state === 'budget_exhausted'} className="bg-sky-400 text-slate-950 hover:bg-sky-300">
              {runningProbe || probeSummary?.config.running ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Play className="mr-2 h-4 w-4" />} 立即探测
            </Button>
          </div>
        </div>
        <ProbeStatusStrip config={probeSummary?.config} />
      </section>

      {error && (
        <div className="flex items-start gap-3 rounded-xl border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-800 dark:border-rose-900 dark:bg-rose-950/40 dark:text-rose-200">
          <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" /><div><div className="font-medium">监测数据加载不完整</div><div className="mt-0.5 opacity-80">{error}</div></div>
        </div>
      )}

      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <MetricCard label="监控模型" value={stats.total} hint={selectedModels.length > 0 ? '固定监控范围' : '按 24h 流量自动展示'} icon={ServerCog} tone="slate" />
        <MetricCard label="需要关注" value={stats.attention} hint="异常和降级优先处理" icon={AlertTriangle} tone={stats.attention > 0 ? 'rose' : 'slate'} />
        <MetricCard label="双源健康" value={stats.healthy} hint="真实流量和主动探测均健康" icon={CheckCircle2} tone="emerald" />
        <MetricCard label="证据不足" value={stats.unknown} hint="无流量、未探测或数据过期" icon={Clock3} tone={stats.unknown > 0 ? 'amber' : 'slate'} />
      </div>

      {attentionRows.length > 0 && (
        <Card className="border-rose-200/80 bg-gradient-to-r from-rose-50 to-white dark:border-rose-950 dark:from-rose-950/30 dark:to-card">
          <CardContent className="p-0">
            <div className="flex items-center justify-between border-b border-rose-100 px-4 py-3 dark:border-rose-950">
              <div><div className="text-sm font-semibold">需要关注</div><div className="text-xs text-muted-foreground">按影响程度排序，只展示最优先的异常</div></div>
              <Badge variant="destructive">{stats.attention}</Badge>
            </div>
            <div className="divide-y divide-border/60">
              {attentionRows.map(row => (
                <button key={row.modelName} onClick={() => openDetail(row.modelName)} className="flex w-full items-center gap-3 px-4 py-3 text-left transition-colors hover:bg-background/70">
                  <span className={cn('h-2.5 w-2.5 shrink-0 rounded-full', combinedMeta[row.combined].dot)} />
                  <div className="min-w-0 flex-1"><div className="truncate text-sm font-medium">{row.modelName}</div><div className="truncate text-xs text-muted-foreground">{row.reasons[0]}</div></div>
                  <span className="hidden text-xs text-muted-foreground sm:block">{relativeTime(row.probe?.latest?.finished_at ?? row.traffic?.last_traffic_at)}</span>
                  <ChevronRight className="h-4 w-4 text-muted-foreground" />
                </button>
              ))}
            </div>
          </CardContent>
        </Card>
      )}

      <section className="overflow-hidden rounded-2xl border border-border/70 bg-card shadow-sm">
        <div className="flex flex-col gap-3 border-b border-border/60 p-4 lg:flex-row lg:items-center lg:justify-between">
          <div className="relative min-w-0 flex-1 lg:max-w-sm">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
            <input aria-label="搜索模型名称" value={search} onChange={event => setSearch(event.target.value)} placeholder="搜索模型名称" className="h-10 w-full rounded-lg border border-input bg-background pl-9 pr-3 text-sm outline-none ring-offset-background focus:ring-2 focus:ring-ring" />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Filter className="h-4 w-4 text-muted-foreground" />
            <select aria-label="按综合状态筛选模型" value={statusFilter} onChange={event => setStatusFilter(event.target.value as typeof statusFilter)} className="h-10 rounded-lg border border-input bg-background px-3 text-sm">
              <option value="all">全部状态</option><option value="unhealthy">确认异常</option><option value="degraded">需要关注</option><option value="healthy">双源健康</option><option value="observing">单源观测</option><option value="unknown">证据不足</option>
            </select>
            <select aria-label="按令牌分组筛选模型" value={groupFilter} onChange={event => setGroupFilter(event.target.value)} className="h-10 max-w-[180px] rounded-lg border border-input bg-background px-3 text-sm">
              <option value="all">全部分组</option>{tokenGroups.map(group => <option key={group.group_name} value={group.group_name}>{group.group_name} ({group.model_count})</option>)}
            </select>
            <select aria-label="选择模型状态时间窗口" value={timeWindow} onChange={event => setTimeWindow(event.target.value)} className="h-10 rounded-lg border border-input bg-background px-3 text-sm">
              {WINDOW_OPTIONS.map(option => <option key={option.value} value={option.value}>{option.label}</option>)}
            </select>
          </div>
        </div>

        <div className="hidden overflow-x-auto lg:block">
          <table className="w-full min-w-[1050px] text-sm">
            <thead className="bg-muted/35 text-left text-xs font-medium uppercase tracking-wide text-muted-foreground">
              <tr><th className="px-4 py-3">模型</th><th className="px-3 py-3">综合状态</th><th className="px-3 py-3">真实流量</th><th className="px-3 py-3">主动探测</th><th className="px-3 py-3 text-right">探测可用率</th><th className="px-3 py-3 text-right">首字延迟</th><th className="px-3 py-3 text-right">总延迟</th><th className="px-3 py-3">最近证据</th><th className="px-4 py-3" /></tr>
            </thead>
            <tbody className="divide-y divide-border/60">
              {visibleRows.map(row => <ModelTableRow key={row.modelName} row={row} onOpen={() => openDetail(row.modelName)} />)}
            </tbody>
          </table>
        </div>
        <div className="divide-y divide-border/60 lg:hidden">
          {visibleRows.map(row => <ModelMobileRow key={row.modelName} row={row} onOpen={() => openDetail(row.modelName)} />)}
        </div>
        {visibleRows.length === 0 && <div className="px-4 py-16 text-center text-sm text-muted-foreground">没有符合当前筛选条件的模型</div>}
      </section>

      <Dialog open={selectorOpen} onOpenChange={setSelectorOpen}>
        {selectorOpen && (
          <ModelSelector available={availableModels} selected={selectorDraft} search={selectorSearch} max={maxBatch} onSearch={setSelectorSearch} onChange={setSelectorDraft} onClose={() => setSelectorOpen(false)} onSave={() => void saveSelection()} onCloseAutoFocus={restoreSelectorFocus} />
        )}
      </Dialog>
      <Dialog open={Boolean(selectedDetail)} onOpenChange={open => { if (!open) setDetailModel(null) }}>
        {selectedDetail && (
          <ModelDetailDrawer row={selectedDetail} history={probeHistory} historyLoading={historyLoading} onClose={() => setDetailModel(null)} onCloseAutoFocus={restoreDetailFocus} />
        )}
      </Dialog>
    </div>
  )
}

function ProbeStatusStrip({ config }: { config?: ProbeConfig }) {
  const state = config?.state ?? 'unavailable'
  const tone = state === 'ready' ? 'text-emerald-600 dark:text-emerald-300'
    : state === 'running' ? 'text-sky-600 dark:text-sky-300'
      : state === 'disabled' || state === 'misconfigured' || state === 'budget_exhausted' ? 'text-amber-600 dark:text-amber-300' : 'text-muted-foreground'
  return (
    <div className="grid gap-3 px-5 py-4 text-sm sm:grid-cols-2 sm:px-7 xl:grid-cols-4">
      <div className="flex items-start gap-3"><Zap className={cn('mt-0.5 h-4 w-4', tone)} /><div><div className="font-medium">主动探测：{stateLabel(state)}</div><div className="mt-0.5 text-xs text-muted-foreground">{config?.message || (config?.configured ? `${config.allowed_models.length} 个白名单模型` : '等待配置')}</div></div></div>
      <div className="flex items-start gap-3"><Gauge className="mt-0.5 h-4 w-4 text-muted-foreground" /><div><div className="font-medium">今日预算 {config ? `${config.requests_used_today}/${config.daily_request_budget}` : '—'}</div><div className="mt-0.5 text-xs text-muted-foreground">剩余 {config?.requests_remaining_today ?? '—'} 次请求</div></div></div>
      <div className="flex items-start gap-3"><TimerReset className="mt-0.5 h-4 w-4 text-muted-foreground" /><div><div className="font-medium">调度间隔 {config ? `${Math.round(config.interval_seconds / 60)} 分钟` : '—'}</div><div className="mt-0.5 text-xs text-muted-foreground">下次：{config?.next_run_at ? formatTime(config.next_run_at) : '未安排'}</div></div></div>
      <div className="flex items-start gap-3"><ServerCog className="mt-0.5 h-4 w-4 text-muted-foreground" /><div><div className="font-medium">并发 {config?.max_concurrency ?? '—'} · 单次上限 {config?.max_models_per_run ?? '—'}</div><div className="mt-0.5 text-xs text-muted-foreground">明细保留 {config?.retention_days ?? '—'} 天</div></div></div>
    </div>
  )
}

function stateLabel(state: ProbeConfig['state']) {
  return ({ disabled: '已关闭', misconfigured: '待配置', ready: '就绪', running: '执行中', budget_exhausted: '预算用尽', unavailable: '不可用' })[state]
}

function MetricCard({ label, value, hint, icon: Icon, tone }: { label: string; value: number; hint: string; icon: typeof Activity; tone: 'slate' | 'rose' | 'emerald' | 'amber' }) {
  const iconTone = { slate: 'bg-slate-100 text-slate-600 dark:bg-slate-900 dark:text-slate-300', rose: 'bg-rose-100 text-rose-600 dark:bg-rose-950 dark:text-rose-300', emerald: 'bg-emerald-100 text-emerald-600 dark:bg-emerald-950 dark:text-emerald-300', amber: 'bg-amber-100 text-amber-600 dark:bg-amber-950 dark:text-amber-300' }[tone]
  return <Card><CardContent className="flex items-center gap-4 p-4"><div className={cn('flex h-10 w-10 items-center justify-center rounded-xl', iconTone)}><Icon className="h-5 w-5" /></div><div className="min-w-0"><div className="text-2xl font-semibold tabular-nums">{value}</div><div className="text-sm font-medium">{label}</div><div className="truncate text-xs text-muted-foreground">{hint}</div></div></CardContent></Card>
}

function CombinedBadge({ health }: { health: CombinedHealth }) {
  const meta = combinedMeta[health]
  return <span className={cn('inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium', meta.badge)}><span className={cn('h-1.5 w-1.5 rounded-full', meta.dot)} />{meta.label}</span>
}

function TrafficCell({ status }: { status?: TrafficStatus }) {
  const health = trafficHealthOf(status)
  const source = sourceStateOf(status)
  const tone = health === 'healthy' ? 'text-emerald-600' : health === 'degraded' ? 'text-amber-600' : health === 'unhealthy' ? 'text-rose-600' : 'text-muted-foreground'
  return <div><div className={cn('font-medium tabular-nums', tone)}>{formatPercent(status?.success_rate)}</div><div className="text-xs text-muted-foreground">{source === 'fresh' ? `${status?.total_requests ?? 0} 次请求` : source === 'stale' ? '数据过期' : '无真实流量'}</div></div>
}

function ProbeCell({ probe }: { probe?: ProbeItem }) {
  const health = probe?.probe_health ?? 'unavailable'
  const tone = health === 'healthy' ? 'text-emerald-600' : health === 'degraded' || health === 'stale' ? 'text-amber-600' : health === 'unhealthy' ? 'text-rose-600' : health === 'pending' ? 'text-sky-600' : 'text-muted-foreground'
  return <div><div className={cn('font-medium', tone)}>{probeStateLabel[health]}</div><div className="max-w-[180px] truncate text-xs text-muted-foreground">{probe?.reason || '尚无主动探测证据'}</div></div>
}

function ModelTableRow({ row, onOpen }: { row: ModelRow; onOpen: () => void }) {
  return (
    <tr className="cursor-pointer transition-colors hover:bg-muted/25" onClick={onOpen}>
      <td className="px-4 py-3"><div className="max-w-[240px] truncate font-medium" title={row.modelName}>{row.modelName}</div><div className="text-xs text-muted-foreground">{row.probe?.capability?.split('_').join(' ') || 'capability unknown'}</div></td>
      <td className="px-3 py-3"><CombinedBadge health={row.combined} /></td>
      <td className="px-3 py-3"><TrafficCell status={row.traffic} /></td>
      <td className="px-3 py-3"><ProbeCell probe={row.probe} /></td>
      <td className="px-3 py-3 text-right font-medium tabular-nums">{formatPercent(row.probe?.availability_24h)}</td>
      <td className="px-3 py-3 text-right tabular-nums">{formatLatency(row.probe?.latest?.first_token_latency_ms ?? row.probe?.average_first_token_latency_ms)}</td>
      <td className="px-3 py-3 text-right tabular-nums">{formatLatency(row.probe?.latest?.total_latency_ms ?? row.probe?.average_total_latency_ms)}</td>
      <td className="px-3 py-3"><div className="text-xs">{relativeTime(row.probe?.latest?.finished_at ?? row.traffic?.last_traffic_at)}</div><div className="text-[11px] text-muted-foreground">{formatTime(row.probe?.latest?.finished_at ?? row.traffic?.last_traffic_at)}</div></td>
      <td className="px-4 py-3 text-right">
        <button
          type="button"
          className="ml-auto inline-flex h-9 w-9 items-center justify-center rounded-md text-muted-foreground hover:bg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          onClick={event => { event.stopPropagation(); onOpen() }}
          aria-label={`打开模型 ${row.modelName} 诊断详情`}
        >
          <ChevronRight className="h-4 w-4" />
        </button>
      </td>
    </tr>
  )
}

function ModelMobileRow({ row, onOpen }: { row: ModelRow; onOpen: () => void }) {
  return <button onClick={onOpen} className="w-full p-4 text-left"><div className="flex items-start justify-between gap-3"><div className="min-w-0"><div className="truncate font-medium">{row.modelName}</div><div className="mt-1 text-xs text-muted-foreground">{row.reasons[0]}</div></div><CombinedBadge health={row.combined} /></div><div className="mt-4 grid grid-cols-3 gap-2 rounded-xl bg-muted/30 p-3 text-xs"><div><div className="text-muted-foreground">真实流量</div><div className="mt-1 font-medium">{formatPercent(row.traffic?.success_rate)}</div></div><div><div className="text-muted-foreground">主动探测</div><div className="mt-1 font-medium">{probeStateLabel[row.probe?.probe_health ?? 'unavailable']}</div></div><div><div className="text-muted-foreground">首字延迟</div><div className="mt-1 font-medium">{formatLatency(row.probe?.latest?.first_token_latency_ms)}</div></div></div></button>
}

function ModelSelector({ available, selected, search, max, onSearch, onChange, onClose, onSave, onCloseAutoFocus }: { available: AvailableModel[]; selected: string[]; search: string; max: number; onSearch: (value: string) => void; onChange: (value: string[]) => void; onClose: () => void; onSave: () => void; onCloseAutoFocus: (event: Event) => void }) {
  const deferred = useDeferredValue(search.toLowerCase())
  const visible = useMemo(() => available.filter(model => model.model_name.toLowerCase().includes(deferred)).slice(0, 300), [available, deferred])
  const selectedSet = useMemo(() => new Set(selected), [selected])
  const toggle = (model: string) => {
    if (selectedSet.has(model)) onChange(selected.filter(item => item !== model))
    else if (selected.length < max) onChange([...selected, model])
  }
  return (
    <DialogContent className="max-w-2xl gap-0 overflow-hidden p-0" onCloseAutoFocus={onCloseAutoFocus}>
      <DialogHeader className="border-b p-4 pr-12">
        <DialogTitle>监控范围</DialogTitle>
        <DialogDescription>已选择 {selected.length}/{max}；清空后自动展示流量最高的模型。</DialogDescription>
      </DialogHeader>
      <div className="border-b p-4">
        <div className="relative">
          <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
          <input autoFocus aria-label="搜索可监控模型" value={search} onChange={event => onSearch(event.target.value)} placeholder="搜索模型" className="h-10 w-full rounded-lg border border-input bg-background pl-9 pr-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring" />
        </div>
      </div>
      <div className="max-h-[55vh] overflow-y-auto p-2">
        {visible.map(model => (
          <label key={model.model_name} className="flex cursor-pointer items-center gap-3 rounded-lg px-3 py-2.5 hover:bg-muted/50">
            <input type="checkbox" checked={selectedSet.has(model.model_name)} onChange={() => toggle(model.model_name)} className="h-4 w-4 rounded border-input" />
            <div className="min-w-0 flex-1"><div className="truncate text-sm font-medium">{model.model_name}</div><div className="text-xs text-muted-foreground">24h 请求 {model.request_count_24h}</div></div>
          </label>
        ))}
      </div>
      <DialogFooter className="flex-row items-center justify-between space-x-0 border-t p-4">
        <Button variant="ghost" onClick={() => onChange([])}>清空</Button>
        <div className="flex gap-2"><Button variant="outline" onClick={onClose}>取消</Button><Button onClick={onSave}>保存范围</Button></div>
      </DialogFooter>
    </DialogContent>
  )
}

function ModelDetailDrawer({ row, history, historyLoading, onClose, onCloseAutoFocus }: { row: ModelRow; history: ProbeAttempt[]; historyLoading: boolean; onClose: () => void; onCloseAutoFocus: (event: Event) => void }) {
  return (
    <DialogContent
      className="bottom-0 left-auto right-0 top-0 h-dvh max-h-none w-full max-w-xl translate-x-0 translate-y-0 gap-0 rounded-none border-y-0 border-r-0 p-0 max-sm:top-0 max-sm:rounded-none"
      onEscapeKeyDown={() => onClose()}
      onCloseAutoFocus={onCloseAutoFocus}
    >
      <DialogHeader className="border-b p-5 pr-12 text-left">
        <div className="mb-2"><CombinedBadge health={row.combined} /></div>
        <DialogTitle className="break-all">{row.modelName}</DialogTitle>
        <DialogDescription>{row.reasons.join('；')}</DialogDescription>
      </DialogHeader>
      <div className="space-y-5 overflow-y-auto p-5">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <DiagnosticCard label="真实流量可用率" value={formatPercent(row.traffic?.success_rate)} detail={`${row.traffic?.total_requests ?? 0} 次请求 · ${sourceStateOf(row.traffic)}`} />
          <DiagnosticCard label="主动探测可用率" value={formatPercent(row.probe?.availability_24h)} detail={`${row.probe?.attempt_count_24h ?? 0} 次探测 · ${row.probe?.capability ?? '未识别'}`} />
          <DiagnosticCard label="首字延迟" value={formatLatency(row.probe?.latest?.first_token_latency_ms ?? row.probe?.average_first_token_latency_ms)} detail={`响应头 ${formatLatency(row.probe?.latest?.header_latency_ms ?? row.probe?.average_header_latency_ms)}`} />
          <DiagnosticCard label="总延迟" value={formatLatency(row.probe?.latest?.total_latency_ms ?? row.probe?.average_total_latency_ms)} detail={`最近 ${relativeTime(row.probe?.latest?.finished_at)}`} />
        </div>
        <div>
          <div className="mb-2 flex items-center justify-between"><h4 className="text-sm font-semibold">真实流量时间槽</h4><span className="text-xs text-muted-foreground">{row.traffic?.time_window ?? '—'}</span></div>
          <div className="flex min-h-12 items-end gap-1 rounded-xl border bg-muted/20 p-3">
            {(row.traffic?.slot_data ?? []).map(slot => (
              <div
                key={slot.slot}
                role="img"
                aria-label={`${formatTime(slot.start_time)}，${slot.total_requests} 次请求，成功率 ${formatPercent(slot.success_rate)}`}
                className={cn('min-w-1 flex-1 rounded-sm', slot.status === 'green' ? 'bg-emerald-500' : slot.status === 'yellow' ? 'bg-amber-500' : slot.status === 'red' ? 'bg-rose-500' : 'bg-slate-200 dark:bg-slate-800')}
                style={{ height: `${slot.total_requests > 0 ? Math.max(10, slot.success_rate * 0.36) : 6}px` }}
              />
            ))}
          </div>
        </div>
        <div>
          <h4 className="mb-2 text-sm font-semibold">最近主动探测</h4>
          {historyLoading ? (
            <div className="flex justify-center py-8" role="status" aria-label="正在加载主动探测历史"><Loader2 className="h-5 w-5 animate-spin" /></div>
          ) : history.length === 0 ? (
            <div className="rounded-xl border border-dashed p-6 text-center text-sm text-muted-foreground">尚无主动探测记录</div>
          ) : (
            <div className="space-y-2">
              {history.map(attempt => (
                <div key={attempt.id} className="rounded-xl border p-3">
                  <div className="flex items-center justify-between gap-3">
                    <div className="flex items-center gap-2"><span className={cn('h-2 w-2 rounded-full', attempt.outcome === 'success' ? 'bg-emerald-500' : attempt.outcome === 'failure' ? 'bg-rose-500' : 'bg-slate-400')} /><span className="text-sm font-medium">{attempt.outcome === 'success' ? '成功' : attempt.outcome === 'failure' ? '失败' : '跳过'}</span><span className="text-xs text-muted-foreground">HTTP {attempt.http_status || '—'}</span></div>
                    <span className="text-xs text-muted-foreground">{formatTime(attempt.finished_at)}</span>
                  </div>
                  <div className="mt-2 grid grid-cols-3 gap-2 text-xs text-muted-foreground"><span>首字 {formatLatency(attempt.first_token_latency_ms)}</span><span>总耗时 {formatLatency(attempt.total_latency_ms)}</span><span>{attempt.error_code || '语义校验通过'}</span></div>
                  {attempt.error_message && <div className="mt-2 rounded-lg bg-rose-50 px-2.5 py-2 text-xs text-rose-700 dark:bg-rose-950/30 dark:text-rose-300">{attempt.error_message}</div>}
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </DialogContent>
  )
}

function DiagnosticCard({ label, value, detail }: { label: string; value: string; detail: string }) {
  return <div className="rounded-xl border bg-card p-3"><div className="text-xs text-muted-foreground">{label}</div><div className="mt-1 text-xl font-semibold tabular-nums">{value}</div><div className="mt-1 truncate text-xs text-muted-foreground">{detail}</div></div>
}

export default ModelStatusConsole
