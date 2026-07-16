import { FormEvent, useCallback, useEffect, useRef, useState } from 'react'
import { AlertTriangle, ChevronRight, Gauge, Loader2, Search, UserRound } from 'lucide-react'
import { apiFetch, createAuthHeaders } from '../lib/api'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card'
import { Input } from './ui/input'
import { Select } from './ui/select'

interface APIEnvelope<T> {
  success: boolean
  data: T
  message?: string
  error?: { message?: string }
}

interface SourceStatus {
  source: string
  grain: string
  freshness: string
  data_source: string
  available: boolean
  reason?: string
  result_count: number
  fallback?: boolean
}

interface SearchResult {
  source: string
  grain: string
  freshness: { basis: string; observed_at: number; unit: string }
  data_source: string
  id: string
  user_id?: number
  label: string
  attributes: Record<string, unknown>
}

interface SearchReport {
  query: string
  generated_at: number
  sources: SourceStatus[]
  results: SearchResult[]
}

interface TimelineEvent {
  source: string
  grain: string
  freshness: { basis: string; observed_at: number; unit: string }
  data_source: string
  event_type: string
  event_id: string
  user_id: number
  summary: string
  details: Record<string, unknown>
}

interface TimelineReport {
  user_id: number
  generated_at: number
  sources: SourceStatus[]
  events: TimelineEvent[]
  next_cursor: string | null
  has_more: boolean
}

interface ChannelMetric {
  channel_id: number
  request_count: number
  success_count: number
  failure_count: number
  success_rate: number
  last_request_at: number
  avg_use_time_seconds: number
  p95_use_time_seconds: number
  confidence: string
  small_sample: boolean
}

interface ChannelQualityReport {
  window: string
  generated_at: number
  data_source: { mode: string; fallback: boolean; healthy: boolean }
  sample: { sampled_rows: number; limit: number; limit_reached: boolean }
  channels: ChannelMetric[]
}

interface ControlPlaneExplorerProps {
  token: string | null
  apiUrl: string
}

async function readEnvelope<T>(response: Response): Promise<T> {
  const body = await response.json() as APIEnvelope<T>
  if (!response.ok || !body.success) {
    throw new Error(body.error?.message || body.message || `HTTP ${response.status}`)
  }
  return body.data
}

function formatObserved(value: number, unit: string): string {
  if (!Number.isFinite(value) || value <= 0) return '时间未知'
  const milliseconds = unit === 'unix_ms' ? value : value * 1000
  return new Date(milliseconds).toLocaleString('zh-CN')
}

function sourceVariant(source: SourceStatus): 'success' | 'warning' | 'secondary' {
  if (!source.available || source.fallback) return 'warning'
  return 'success'
}

export function ControlPlaneExplorer({ token, apiUrl }: ControlPlaneExplorerProps) {
  const [windowKey, setWindowKey] = useState('24h')
  const [channelQuality, setChannelQuality] = useState<ChannelQualityReport | null>(null)
  const [channelLoading, setChannelLoading] = useState(true)
  const [channelError, setChannelError] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  const [searchReport, setSearchReport] = useState<SearchReport | null>(null)
  const [searchLoading, setSearchLoading] = useState(false)
  const [searchError, setSearchError] = useState<string | null>(null)
  const [timeline, setTimeline] = useState<TimelineReport | null>(null)
  const [timelineLoading, setTimelineLoading] = useState(false)
  const [timelineError, setTimelineError] = useState<string | null>(null)
  const searchController = useRef<AbortController | null>(null)
  const timelineController = useRef<AbortController | null>(null)

  const loadChannelQuality = useCallback(async (signal: AbortSignal) => {
    setChannelLoading(true)
    setChannelError(null)
    try {
      const report = await readEnvelope<ChannelQualityReport>(await apiFetch(
        `${apiUrl}/api/control-plane/channel-quality?window=${windowKey}`,
        { headers: createAuthHeaders(token), signal },
      ))
      if (!signal.aborted) setChannelQuality(report)
    } catch (error) {
      if (!signal.aborted) {
        setChannelQuality(null)
        setChannelError(error instanceof Error ? error.message : '渠道质量加载失败')
      }
    } finally {
      if (!signal.aborted) setChannelLoading(false)
    }
  }, [apiUrl, token, windowKey])

  useEffect(() => {
    const controller = new AbortController()
    void loadChannelQuality(controller.signal)
    return () => controller.abort()
  }, [loadChannelQuality])

  useEffect(() => () => {
    searchController.current?.abort()
    timelineController.current?.abort()
  }, [])

  const runSearch = async (event: FormEvent) => {
    event.preventDefault()
    const normalized = query.trim()
    if (normalized.length < 2) {
      setSearchError('请输入至少 2 个字符')
      return
    }
    searchController.current?.abort()
    timelineController.current?.abort()
    timelineController.current = null
    const controller = new AbortController()
    searchController.current = controller
    setSearchLoading(true)
    setSearchError(null)
    setTimeline(null)
    setTimelineLoading(false)
    setTimelineError(null)
    try {
      const report = await readEnvelope<SearchReport>(await apiFetch(
        `${apiUrl}/api/control-plane/search?q=${encodeURIComponent(normalized)}&limit=20`,
        { headers: createAuthHeaders(token), signal: controller.signal },
      ))
      if (!controller.signal.aborted) setSearchReport(report)
    } catch (error) {
      if (!controller.signal.aborted) {
        setSearchReport(null)
        setSearchError(error instanceof Error ? error.message : '搜索失败')
      }
    } finally {
      if (!controller.signal.aborted) setSearchLoading(false)
    }
  }

  const loadTimeline = async (userID: number, cursor?: string, append = false) => {
    timelineController.current?.abort()
    const controller = new AbortController()
    timelineController.current = controller
    setTimelineLoading(true)
    setTimelineError(null)
    const params = new URLSearchParams({ limit: '50' })
    if (cursor) params.set('before', cursor)
    try {
      const report = await readEnvelope<TimelineReport>(await apiFetch(
        `${apiUrl}/api/control-plane/users/${userID}/timeline?${params.toString()}`,
        { headers: createAuthHeaders(token), signal: controller.signal },
      ))
      if (controller.signal.aborted) return
      setTimeline((previous) => append && previous?.user_id === userID
        ? { ...report, events: [...previous.events, ...report.events] }
        : report)
    } catch (error) {
      if (!controller.signal.aborted) {
        setTimelineError(error instanceof Error ? error.message : '时间线加载失败')
      }
    } finally {
      if (!controller.signal.aborted) setTimelineLoading(false)
    }
  }

  return (
    <div className="grid grid-cols-1 xl:grid-cols-2 gap-6">
      <Card>
        <CardHeader>
          <div className="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-3">
            <div>
              <CardTitle className="flex items-center gap-2 text-lg">
                <Gauge className="h-5 w-5 text-primary" />
                渠道质量
              </CardTitle>
              <CardDescription>成功率和延迟来自日志样本，不自动切换渠道。</CardDescription>
            </div>
            <Select
              value={windowKey}
              onChange={(event) => setWindowKey(event.target.value)}
              aria-label="渠道质量时间窗口"
              className="w-full sm:w-28"
            >
              <option value="1h">1 小时</option>
              <option value="24h">24 小时</option>
              <option value="7d">7 天</option>
            </Select>
          </div>
        </CardHeader>
        <CardContent className="space-y-3">
          {channelError ? <InlineError message={channelError} /> : null}
          {channelQuality ? (
            <>
              <div className="flex flex-wrap gap-2 text-xs">
                <Badge variant={channelQuality.data_source.healthy ? 'success' : 'warning'}>
                  {channelQuality.data_source.mode || '日志源未知'}
                </Badge>
                {channelQuality.data_source.fallback ? <Badge variant="warning">回退日志源</Badge> : null}
                <Badge variant="secondary">样本 {channelQuality.sample.sampled_rows.toLocaleString()}</Badge>
                {channelQuality.sample.limit_reached ? <Badge variant="warning">达到采样上限</Badge> : null}
              </div>
              <div className="space-y-2 max-h-[420px] overflow-y-auto pr-1">
                {channelQuality.channels.slice(0, 20).map((channel) => (
                  <div key={channel.channel_id} className="rounded-lg border p-3">
                    <div className="flex items-start justify-between gap-3">
                      <div>
                        <p className="font-medium">渠道 #{channel.channel_id}</p>
                        <p className="text-xs text-muted-foreground mt-0.5">
                          {channel.request_count.toLocaleString()} 次请求 · {channel.failure_count.toLocaleString()} 次失败
                        </p>
                      </div>
                      <Badge variant={channel.success_rate >= 99 ? 'success' : channel.success_rate >= 95 ? 'warning' : 'destructive'}>
                        {channel.success_rate.toFixed(2)}%
                      </Badge>
                    </div>
                    <div className="grid grid-cols-3 gap-2 mt-3 text-xs">
                      <Metric label="平均" value={`${channel.avg_use_time_seconds.toFixed(3)}s`} />
                      <Metric label="P95" value={`${channel.p95_use_time_seconds.toFixed(3)}s`} />
                      <Metric label="置信度" value={channel.confidence} warning={channel.small_sample} />
                    </div>
                  </div>
                ))}
                {channelQuality.channels.length > 20 ? (
                  <p className="py-2 text-center text-xs text-muted-foreground">
                    仅显示前 20 个渠道，共 {channelQuality.channels.length} 个。
                  </p>
                ) : null}
                {channelQuality.channels.length === 0 && !channelLoading ? (
                  <p className="py-8 text-center text-sm text-muted-foreground">当前窗口没有可用渠道样本</p>
                ) : null}
              </div>
            </>
          ) : channelLoading ? <LoadingLabel text="正在读取渠道样本..." /> : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-lg">
            <Search className="h-5 w-5 text-primary" />
            全局搜索与用户时间线
          </CardTitle>
          <CardDescription>并行检索用户、充值和请求日志；选择用户后合并审计、案件与备注。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <form onSubmit={runSearch} className="flex gap-2">
            <Input
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="用户名、用户 ID、交易号或请求 ID"
              aria-label="全局搜索关键词"
              maxLength={128}
              autoComplete="off"
            />
            <Button type="submit" disabled={searchLoading || query.trim().length < 2}>
              {searchLoading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Search className="h-4 w-4" />}
              <span className="sr-only">搜索</span>
            </Button>
          </form>
          {searchError ? <InlineError message={searchError} /> : null}
          {searchReport ? (
            <>
              <SourceBadges sources={searchReport.sources} />
              <div className="space-y-2 max-h-60 overflow-y-auto pr-1">
                {searchReport.results.map((result) => (
                  <button
                    type="button"
                    key={`${result.source}:${result.id}`}
                    onClick={() => result.user_id ? void loadTimeline(result.user_id) : undefined}
                    disabled={!result.user_id}
                    className="w-full rounded-lg border p-3 text-left transition-colors enabled:hover:bg-muted/50 disabled:cursor-default"
                  >
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <p className="font-medium truncate">{result.label || result.id}</p>
                        <p className="text-xs text-muted-foreground mt-0.5">
                          {result.source} · {formatObserved(result.freshness.observed_at, result.freshness.unit)}
                        </p>
                      </div>
                      {result.user_id ? (
                        <span className="flex items-center gap-1 text-xs text-primary shrink-0">
                          用户 #{result.user_id}<ChevronRight className="h-3.5 w-3.5" />
                        </span>
                      ) : null}
                    </div>
                  </button>
                ))}
                {searchReport.results.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">没有匹配结果</p>
                ) : null}
              </div>
            </>
          ) : null}

          {timelineError ? <InlineError message={timelineError} /> : null}
          {timeline ? (
            <div className="space-y-3 border-t pt-4">
              <div className="flex items-center justify-between gap-3">
                <div className="flex items-center gap-2">
                  <UserRound className="h-4 w-4 text-primary" />
                  <h3 className="font-medium">用户 #{timeline.user_id} 时间线</h3>
                </div>
                <Badge variant="secondary">{timeline.events.length} 条</Badge>
              </div>
              <SourceBadges sources={timeline.sources} />
              <div className="space-y-2 max-h-80 overflow-y-auto pr-1">
                {timeline.events.map((event) => (
                  <div key={`${event.source}:${event.event_id}`} className="rounded-lg border p-3">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <p className="text-sm font-medium break-words">{event.summary || event.event_type}</p>
                        <p className="text-xs text-muted-foreground mt-1">{event.source} · {event.event_type}</p>
                      </div>
                      <span className="text-[11px] text-muted-foreground shrink-0">
                        {formatObserved(event.freshness.observed_at, event.freshness.unit)}
                      </span>
                    </div>
                  </div>
                ))}
              </div>
              {timeline.has_more && timeline.next_cursor ? (
                <Button
                  variant="outline"
                  size="sm"
                  className="w-full"
                  disabled={timelineLoading}
                  onClick={() => void loadTimeline(timeline.user_id, timeline.next_cursor || undefined, true)}
                >
                  {timelineLoading ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : null}
                  加载更早记录
                </Button>
              ) : null}
            </div>
          ) : timelineLoading ? <LoadingLabel text="正在合并用户时间线..." /> : null}
        </CardContent>
      </Card>
    </div>
  )
}

function SourceBadges({ sources }: { sources: SourceStatus[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {sources.map((source) => (
        <Badge key={source.source} variant={sourceVariant(source)}>
          {source.source}: {source.available ? source.result_count : source.reason || '不可用'}
          {source.fallback ? ' · 回退' : ''}
        </Badge>
      ))}
    </div>
  )
}

function Metric({ label, value, warning = false }: { label: string; value: string; warning?: boolean }) {
  return (
    <div className="rounded-md bg-muted/40 px-2 py-1.5">
      <p className="text-muted-foreground">{label}</p>
      <p className={warning ? 'font-medium text-amber-600' : 'font-medium'}>{value}</p>
    </div>
  )
}

function InlineError({ message }: { message: string }) {
  return (
    <div className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 p-3 text-xs text-destructive">
      <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
      <span>{message}</span>
    </div>
  )
}

function LoadingLabel({ text }: { text: string }) {
  return (
    <div className="flex items-center justify-center gap-2 py-8 text-sm text-muted-foreground">
      <Loader2 className="h-4 w-4 animate-spin" />
      {text}
    </div>
  )
}
