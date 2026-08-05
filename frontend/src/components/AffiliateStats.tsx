import { useState, useEffect, useCallback, useMemo, useRef } from 'react'
import {
  Users,
  Loader2,
  RefreshCw,
  Search,
  Filter,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  TrendingUp,
  UserCheck,
  Receipt,
  ShieldAlert,
  History,
} from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { Input } from './ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from './ui/table'
import { StatCard } from './StatCard'
import { useToast } from './Toast'
import { useAuth } from '../contexts/AuthContext'
import { cn } from '../lib/utils'
import {
  equalSha256Hex,
  inviteTopUpQueryFingerprint,
  isSha256Hex,
} from '../lib/affiliateFingerprint'

type RequestState = 'loading' | 'fresh' | 'stale' | 'unavailable' | 'empty'
type SourceState = 'empty' | 'unreconciled'

interface AnalysisMetadata {
  currency: string
  unit: string
  source_state: SourceState
  as_of: number
  query_fingerprint: string
}

interface InviteTopUpRow {
  inviter_id: number
  inviter_username: string | null
  inviter_display_name: string | null
  current_invitee_count: number
  window_paying_invitee_count: number
  rewarded_invite_count: number
  success_topup_count: number
  success_amount: number | null
  success_money: number | null
  last_topup_at: number | null
  detail_evidence_hash: string
}

interface InviteTopUpSummary extends AnalysisMetadata {
  window_active_inviter_count: number
  current_invitee_count: number
  window_paying_invitee_count: number
  rewarded_invite_count: number
  success_topup_count: number
  total_amount: number | null
  total_money: number | null
}

interface PaginatedAnalysis extends AnalysisMetadata {
  items: InviteTopUpRow[]
  summary: InviteTopUpSummary
  total: number
  page: number
  page_size: number
  total_pages: number
}

interface TopUpDetailRow {
  id: number
  user_id: number
  username: string | null
  amount: number | null
  money: number | null
  complete_time: number
  status: string
}

interface PaginatedDetails extends AnalysisMetadata {
  inviter_id: number
  items: TopUpDetailRow[]
  total: number
  page: number
  page_size: number
  total_pages: number
  total_amount: number | null
  total_money: number | null
  detail_evidence_hash: string
}

type SortBy =
  | 'success_topup_count'
  | 'window_paying_invitee_count'
  | 'last_topup_at'
  | 'current_invitee_count'
  | 'rewarded_invite_count'

const SORTABLE: Record<SortBy, string> = {
  success_topup_count: '窗口充值笔数',
  window_paying_invitee_count: '窗口充值用户',
  last_topup_at: '最近完成时间',
  current_invitee_count: '当前邀请用户',
  rewarded_invite_count: '奖励计数旁证',
}

const isAbortError = (error: unknown) => error instanceof DOMException && error.name === 'AbortError'
const formatTime = (timestamp: number | null | undefined) => timestamp
  ? new Date(timestamp * 1000).toLocaleString('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit',
  })
  : '-'
const formatRawValue = (value: number | null) => value === null ? '未提供' : value.toLocaleString()

function stateMessage(state: RequestState, label: string) {
  if (state === 'unavailable') return `${label}暂不可用，未显示为 0 或空数据。`
  if (state === 'stale') return `${label}刷新失败，当前保留的是上次成功数据。`
  return ''
}

export function AffiliateStats() {
  const { showToast } = useToast()
  const { token } = useAuth()

  const [rows, setRows] = useState<InviteTopUpRow[]>([])
  const [summary, setSummary] = useState<InviteTopUpSummary | null>(null)
  const [listMetadata, setListMetadata] = useState<AnalysisMetadata | null>(null)
  const [listState, setListState] = useState<RequestState>('loading')
  const [summaryState, setSummaryState] = useState<RequestState>('loading')
  const [refreshing, setRefreshing] = useState(false)

  const [page, setPage] = useState(1)
  const pageSize = 20
  const [total, setTotal] = useState(0)
  const [totalPages, setTotalPages] = useState(1)

  const [search, setSearch] = useState('')
  const [searchInput, setSearchInput] = useState('')
  const [startDate, setStartDate] = useState('')
  const [endDate, setEndDate] = useState('')
  const [sortBy, setSortBy] = useState<SortBy>('success_topup_count')
  const [sortDir, setSortDir] = useState<'asc' | 'desc'>('desc')
  const [asOf, setAsOf] = useState(() => Math.floor(Date.now() / 1000))

  const [expandedId, setExpandedId] = useState<number | null>(null)
  const [detailState, setDetailState] = useState<RequestState>('empty')
  const [detailData, setDetailData] = useState<PaginatedDetails | null>(null)
  const [detailPage, setDetailPage] = useState(1)

  const listControllerRef = useRef<AbortController | null>(null)
  const detailControllerRef = useRef<AbortController | null>(null)
  const preserveListNextRef = useRef(false)

  const apiUrl = import.meta.env.VITE_API_URL || ''
  const authHeaders = useMemo(() => ({
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }), [token])

  const buildParentParams = useCallback((snapshotAsOf = asOf) => {
    const params = new URLSearchParams()
    if (search) params.append('search', search)
    if (startDate) params.append('start_date', startDate)
    if (endDate) params.append('end_date', endDate)
    params.append('sort_by', sortBy)
    params.append('sort_dir', sortDir)
    params.append('as_of', String(snapshotAsOf))
    return params
  }, [asOf, endDate, search, sortBy, sortDir, startDate])

  const clearDetail = useCallback(() => {
    detailControllerRef.current?.abort()
    detailControllerRef.current = null
    setExpandedId(null)
    setDetailData(null)
    setDetailPage(1)
    setDetailState('empty')
  }, [])

  const beginNewQuery = useCallback(() => {
    listControllerRef.current?.abort()
    preserveListNextRef.current = false
    setRows([])
    setSummary(null)
    setListMetadata(null)
    setTotal(0)
    setTotalPages(1)
    setListState('loading')
    setSummaryState('loading')
    setPage(1)
    clearDetail()
    setAsOf(current => Math.max(Math.floor(Date.now() / 1000), current + 1))
  }, [clearDetail])

  const fetchList = useCallback(async (preserve: boolean): Promise<boolean> => {
    listControllerRef.current?.abort()
    const controller = new AbortController()
    listControllerRef.current = controller
    setListState('loading')
    setSummaryState('loading')
    if (!preserve) {
      setRows([])
      setSummary(null)
      setListMetadata(null)
      setTotal(0)
      setTotalPages(1)
    }
    try {
      const expectedFingerprint = await inviteTopUpQueryFingerprint({
        search,
        startDate,
        endDate,
        sortBy,
        sortDir,
        asOf,
      })
      if (controller.signal.aborted || listControllerRef.current !== controller) return false
      const params = buildParentParams()
      params.append('page', String(page))
      params.append('page_size', String(pageSize))
      const response = await fetch(`${apiUrl}/api/users/invite-topup-analysis?${params.toString()}`, {
        headers: authHeaders,
        signal: controller.signal,
      })
      const body = await response.json()
      if (controller.signal.aborted || listControllerRef.current !== controller) return false
      if (!response.ok || !body.success) throw new Error(body.error?.message || '邀请充值分析列表不可用')
      const result = body.data as PaginatedAnalysis
      if (!equalSha256Hex(result?.query_fingerprint, expectedFingerprint) || result.as_of !== asOf || result.page !== page || result.page_size !== pageSize || !Array.isArray(result.items) ||
        result.items.some(row => !isSha256Hex(row?.detail_evidence_hash)) ||
        !result.summary || !equalSha256Hex(result.summary.query_fingerprint, expectedFingerprint) || result.summary.as_of !== result.as_of) {
        throw new Error('列表响应缺少查询身份')
      }
      setRows(result.items)
      setSummary(result.summary)
      setListMetadata(result)
      setTotal(result.total)
      setTotalPages(result.total_pages)
      setListState(result.items.length === 0 ? 'empty' : 'fresh')
      setSummaryState(result.summary.success_topup_count === 0 ? 'empty' : 'fresh')
      return true
    } catch (error) {
      if (controller.signal.aborted || isAbortError(error)) return false
      console.error('Failed to fetch invite top-up analysis:', error)
      setListState(preserve ? 'stale' : 'unavailable')
      setSummaryState(preserve ? 'stale' : 'unavailable')
      return false
    } finally {
      if (listControllerRef.current === controller) listControllerRef.current = null
    }
  }, [apiUrl, asOf, authHeaders, buildParentParams, endDate, page, search, sortBy, sortDir, startDate])

  useEffect(() => {
    const preserve = preserveListNextRef.current
    preserveListNextRef.current = false
    void fetchList(preserve)
  }, [fetchList])

  useEffect(() => () => {
    listControllerRef.current?.abort()
    detailControllerRef.current?.abort()
  }, [])

  useEffect(() => {
    if (!refreshing || listState === 'loading' || summaryState === 'loading') return
    setRefreshing(false)
    if ((listState === 'fresh' || listState === 'empty') && (summaryState === 'fresh' || summaryState === 'empty')) {
      showToast('success', '邀请充值分析已刷新')
    } else {
      showToast('error', '刷新未完整成功，旧数据已明确标记')
    }
  }, [listState, refreshing, showToast, summaryState])

  const handleRefresh = () => {
    preserveListNextRef.current = true
    clearDetail()
    setRefreshing(true)
    setAsOf(current => Math.max(Math.floor(Date.now() / 1000), current + 1))
  }

  const applySearch = () => {
    const next = searchInput.trim()
    beginNewQuery()
    setSearch(next)
  }

  const changeStartDate = (value: string) => {
    beginNewQuery()
    setStartDate(value)
  }

  const changeEndDate = (value: string) => {
    beginNewQuery()
    setEndDate(value)
  }

  const clearSearchFilter = () => {
    beginNewQuery()
    setSearch('')
    setSearchInput('')
  }

  const clearStartDateFilter = () => {
    beginNewQuery()
    setStartDate('')
  }

  const clearEndDateFilter = () => {
    beginNewQuery()
    setEndDate('')
  }

  const clearAllFilters = () => {
    beginNewQuery()
    setSearch('')
    setSearchInput('')
    setStartDate('')
    setEndDate('')
  }

  const toggleSort = (column: SortBy) => {
    beginNewQuery()
    if (sortBy === column) {
      setSortDir(direction => direction === 'asc' ? 'desc' : 'asc')
    } else {
      setSortBy(column)
      setSortDir('desc')
    }
  }

  const changePage = (nextPage: number) => {
    listControllerRef.current?.abort()
    setRows([])
    setListState('loading')
    clearDetail()
    setPage(nextPage)
  }

  const fetchDetails = async (inviterId: number, requestedPage: number) => {
    const parentRow = rows.find(row => row.inviter_id === inviterId)
    const expectedTotal = parentRow?.success_topup_count
    const expectedEvidenceHash = parentRow?.detail_evidence_hash
    if (!isSha256Hex(listMetadata?.query_fingerprint) || expectedTotal === undefined || !isSha256Hex(expectedEvidenceHash)) {
      setDetailState('unavailable')
      return
    }
    detailControllerRef.current?.abort()
    const controller = new AbortController()
    detailControllerRef.current = controller
    setExpandedId(inviterId)
    setDetailPage(requestedPage)
    setDetailData(null)
    setDetailState('loading')
    try {
      const params = buildParentParams()
      params.append('page', String(requestedPage))
      params.append('page_size', '10')
      params.append('query_fingerprint', listMetadata.query_fingerprint)
      params.append('expected_total', String(expectedTotal))
      params.append('expected_evidence_hash', expectedEvidenceHash)
      const response = await fetch(
        `${apiUrl}/api/users/invite-topup-analysis/${inviterId}/details?${params.toString()}`,
        { headers: authHeaders, signal: controller.signal },
      )
      const body = await response.json()
      if (controller.signal.aborted || detailControllerRef.current !== controller) return
      if (!response.ok || !body.success) throw new Error(body.error?.message || '逐笔源记录不可用')
      const result = body.data as PaginatedDetails
      if (result.inviter_id !== inviterId || result.query_fingerprint !== listMetadata.query_fingerprint || !isSha256Hex(result.detail_evidence_hash) || result.detail_evidence_hash !== expectedEvidenceHash || result.as_of !== asOf || result.page !== requestedPage || result.page_size !== 10 || result.total !== expectedTotal) {
        throw new Error('详情响应与当前父查询不一致')
      }
      setDetailData(result)
      setDetailState(result.items.length === 0 ? 'empty' : 'fresh')
    } catch (error) {
      if (controller.signal.aborted || isAbortError(error)) return
      console.error('Failed to fetch invite top-up details:', error)
      setDetailState('unavailable')
      showToast('error', '逐笔源记录不可用，未显示为空记录')
    } finally {
      if (detailControllerRef.current === controller) detailControllerRef.current = null
    }
  }

  const toggleExpand = (inviterId: number) => {
    if (expandedId === inviterId) {
      clearDetail()
      return
    }
    void fetchDetails(inviterId, 1)
  }

  const sortIndicator = (column: SortBy) => sortBy === column
    ? <span aria-hidden="true" className="ml-1 text-xs text-muted-foreground">{sortDir === 'asc' ? '↑' : '↓'}</span>
    : null

  const sortButtonLabel = (column: SortBy) => sortBy === column
    ? `按${SORTABLE[column]}排序，当前${sortDir === 'asc' ? '升序' : '降序'}，激活后切换为${sortDir === 'asc' ? '降序' : '升序'}`
    : `按${SORTABLE[column]}排序`

  const metadataMismatch = Boolean(
    summary && listMetadata && summary.query_fingerprint !== listMetadata.query_fingerprint,
  )

  const summaryValue = (value: number | undefined, suffix: string) => {
    if (summaryState === 'loading') return '-'
    if (summaryState === 'unavailable' || metadataMismatch || value === undefined) return '不可用'
    return `${value.toLocaleString()} ${suffix}`
  }

  const sourceUnreconciled = summary?.source_state === 'unreconciled' || listMetadata?.source_state === 'unreconciled'

  return (
    <div className="min-w-0 max-w-full space-y-6 animate-in fade-in duration-300 motion-reduce:animate-none">
      <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
        <div>
          <h3 className="text-xl font-bold tracking-tight">邀请充值分析</h3>
          <p className="text-muted-foreground text-sm mt-1">
            按当前邀请关系观察完成的充值记录；本页不计算返利，也不用于结算
          </p>
        </div>
        <Button variant="outline" size="sm" onClick={handleRefresh} disabled={refreshing || listState === 'loading' || summaryState === 'loading'} className="h-9">
          <RefreshCw aria-hidden="true" className={cn('h-4 w-4 mr-2', refreshing && 'animate-spin motion-reduce:animate-none')} />刷新
        </Button>
      </div>

      {(sourceUnreconciled || metadataMismatch) && (
        <div role="status" className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-900 dark:text-amber-100">
          <div className="flex items-start gap-2">
            <ShieldAlert aria-hidden="true" className="h-4 w-4 mt-0.5 shrink-0" />
            <div>
              <p className="font-medium">当前来源未对账</p>
              <p className="mt-1 text-xs">
                邀请人来自当前 users.inviter_id，并非充值发生时的快照；top_ups 也没有可验证的币种和入账单位，因此金额与额度不做跨记录汇总。
              </p>
              {metadataMismatch && <p className="mt-1 text-xs font-medium">列表与汇总查询身份不一致，已禁止将两者视为同一快照。</p>}
              {(summary?.as_of || listMetadata?.as_of) && <p className="mt-1 text-xs">数据截止：{formatTime(summary?.as_of || listMetadata?.as_of)}</p>}
            </div>
          </div>
        </div>
      )}

      {(listState === 'unavailable' || listState === 'stale' || summaryState === 'unavailable' || summaryState === 'stale') && (
        <div role="alert" className="rounded-lg border border-destructive/40 bg-destructive/5 p-3 text-sm text-destructive">
          {[stateMessage(listState, '列表'), stateMessage(summaryState, '汇总')].filter(Boolean).join(' ')}
        </div>
      )}

      <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-5 gap-4">
        <StatCard title="窗口充值邀请人" value={summaryValue(summary?.window_active_inviter_count, '人')} icon={Users} color="indigo" className="border-l-4 border-l-indigo-500" />
        <StatCard title="当前邀请用户" value={summaryValue(summary?.current_invitee_count, '人')} icon={UserCheck} color="cyan" className="border-l-4 border-l-cyan-500" />
        <StatCard title="窗口充值用户" value={summaryValue(summary?.window_paying_invitee_count, '人')} icon={TrendingUp} color="emerald" className="border-l-4 border-l-emerald-500" />
        <StatCard title="奖励计数旁证" value={summaryValue(summary?.rewarded_invite_count, '人')} subValue="users.aff_count，非结算金额" icon={History} color="amber" className="border-l-4 border-l-amber-500" />
        <StatCard title="窗口成功充值" value={summaryValue(summary?.success_topup_count, '笔')} subValue="success / completed / 1" icon={Receipt} color="rose" className="border-l-4 border-l-rose-500" />
      </div>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-base font-medium flex items-center gap-2"><Filter aria-hidden="true" className="w-4 h-4" />筛选条件</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-3">
            <div className="relative">
              <label className="sr-only" htmlFor="affiliate-inviter-search">搜索邀请人</label>
              <Search aria-hidden="true" className="absolute left-3 top-1/2 -translate-y-1/2 h-4 w-4 text-muted-foreground" />
              <Input
                id="affiliate-inviter-search"
                placeholder="搜索邀请人 username / display_name"
                value={searchInput}
                onChange={event => setSearchInput(event.target.value)}
                onKeyDown={event => { if (event.key === 'Enter') applySearch() }}
                className="pl-9 h-9"
              />
            </div>
            <div>
              <label className="sr-only" htmlFor="affiliate-start-date">完成日期起始</label>
              <Input id="affiliate-start-date" type="date" value={startDate} onChange={event => changeStartDate(event.target.value)} className="h-9" />
            </div>
            <div>
              <label className="sr-only" htmlFor="affiliate-end-date">完成日期结束（含当日）</label>
              <Input id="affiliate-end-date" type="date" value={endDate} onChange={event => changeEndDate(event.target.value)} className="h-9" />
            </div>
            <Button type="button" onClick={applySearch} variant="default" size="sm" className="h-9 motion-reduce:transition-none"><Search aria-hidden="true" className="h-4 w-4 mr-2" />应用搜索</Button>
          </div>
          <p className="mt-2 text-xs text-muted-foreground">日期按服务端明确时区的完成时间筛选，结束日期规范化为下一日零点，使用半开区间。</p>
          {(search || startDate || endDate) && (
            <div role="group" className="mt-3 flex flex-wrap items-center gap-2" aria-label="已应用的邀请充值筛选">
              <span className="text-xs text-muted-foreground">已应用：</span>
              {search && (
                <Button type="button" variant="outline" size="sm" className="h-7 max-w-full text-xs motion-reduce:transition-none" onClick={clearSearchFilter} aria-label={`移除搜索筛选：${search}`}>
                  <span className="block min-w-0 truncate">搜索：{search}</span><span aria-hidden="true" className="ml-1">×</span>
                </Button>
              )}
              {startDate && (
                <Button type="button" variant="outline" size="sm" className="h-7 max-w-full text-xs motion-reduce:transition-none" onClick={clearStartDateFilter} aria-label={`移除开始日期筛选：${startDate}`}>
                  开始：{startDate}<span aria-hidden="true" className="ml-1">×</span>
                </Button>
              )}
              {endDate && (
                <Button type="button" variant="outline" size="sm" className="h-7 max-w-full text-xs motion-reduce:transition-none" onClick={clearEndDateFilter} aria-label={`移除结束日期筛选：${endDate}`}>
                  结束：{endDate}<span aria-hidden="true" className="ml-1">×</span>
                </Button>
              )}
              <Button type="button" variant="ghost" size="sm" className="h-7 text-xs motion-reduce:transition-none" onClick={clearAllFilters}>清除全部筛选</Button>
            </div>
          )}
          <div className="mt-3 flex flex-wrap gap-2 text-xs text-muted-foreground">
            <span>排序：</span>
            {(Object.keys(SORTABLE) as SortBy[]).map(column => (
              <button
                key={column}
                type="button"
                onClick={() => toggleSort(column)}
                aria-label={sortButtonLabel(column)}
                aria-pressed={sortBy === column}
                className={cn('px-2 py-0.5 rounded border transition-colors motion-reduce:transition-none focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2', sortBy === column ? 'bg-primary text-primary-foreground border-primary' : 'hover:bg-muted')}
              >
                {SORTABLE[column]}{sortIndicator(column)}
              </button>
            ))}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-3 flex flex-row items-center justify-between">
          <CardTitle className="min-w-0 flex flex-wrap items-center gap-2 text-base font-medium">
            <TrendingUp aria-hidden="true" className="w-4 h-4" />邀请充值明细
            {listState !== 'unavailable' && <Badge variant="outline" className="ml-2">{total} 个邀请人</Badge>}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {listState === 'loading' ? (
            <div className="flex items-center justify-center py-12 text-muted-foreground"><Loader2 aria-hidden="true" className="h-5 w-5 mr-2 animate-spin motion-reduce:animate-none" />加载中...</div>
          ) : listState === 'unavailable' ? (
            <div className="py-12 text-center text-destructive text-sm">列表不可用；这不是“暂无数据”。</div>
          ) : rows.length === 0 ? (
            <div className="py-12 text-center text-muted-foreground text-sm">当前查询条件下没有完成的邀请充值记录</div>
          ) : (
            <div className="max-w-full overflow-x-auto">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="w-8"><span className="sr-only">逐笔记录</span></TableHead>
                    <TableHead>邀请人</TableHead>
                    {(['current_invitee_count', 'rewarded_invite_count', 'window_paying_invitee_count'] as SortBy[]).map(column => (
                      <TableHead
                        key={column}
                        className="text-right"
                        aria-sort={sortBy === column ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}
                      >
                        <button type="button" onClick={() => toggleSort(column)} aria-label={sortButtonLabel(column)} className="inline-flex w-full items-center justify-end rounded-sm py-2 text-right focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2">
                          {SORTABLE[column]}{sortIndicator(column)}
                        </button>
                      </TableHead>
                    ))}
                    <TableHead className="text-right" aria-sort={sortBy === 'success_topup_count' ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}>
                      <button type="button" onClick={() => toggleSort('success_topup_count')} aria-label={sortButtonLabel('success_topup_count')} className="inline-flex w-full items-center justify-end rounded-sm py-2 text-right focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2">
                        窗口充值笔数{sortIndicator('success_topup_count')}
                      </button>
                    </TableHead>
                    <TableHead aria-sort={sortBy === 'last_topup_at' ? (sortDir === 'asc' ? 'ascending' : 'descending') : 'none'}>
                      <button type="button" onClick={() => toggleSort('last_topup_at')} aria-label={sortButtonLabel('last_topup_at')} className="inline-flex items-center rounded-sm py-2 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2">
                        最近完成时间{sortIndicator('last_topup_at')}
                      </button>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.flatMap(row => {
                    const main = (
                      <TableRow
                        key={row.inviter_id}
                        className="hover:bg-muted/40 motion-reduce:transition-none"
                      >
                        <TableCell>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            className="h-8 w-8 p-0 motion-reduce:transition-none"
                            onClick={() => toggleExpand(row.inviter_id)}
                            aria-label={`${expandedId === row.inviter_id ? '收起' : '展开'}邀请人 ${row.inviter_username || `#${row.inviter_id}`} 的逐笔充值记录`}
                            aria-expanded={expandedId === row.inviter_id}
                            aria-controls={`invite-topup-detail-${row.inviter_id}`}
                          >
                            {expandedId === row.inviter_id
                              ? <ChevronDown aria-hidden="true" className="h-4 w-4 text-muted-foreground" />
                              : <ChevronRight aria-hidden="true" className="h-4 w-4 text-muted-foreground" />}
                          </Button>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-col">
                            <span className="font-medium">{row.inviter_username || `#${row.inviter_id}`}</span>
                            {row.inviter_display_name && row.inviter_display_name !== row.inviter_username && <span className="text-xs text-muted-foreground">{row.inviter_display_name}</span>}
                            <span className="text-[10px] text-muted-foreground">ID: {row.inviter_id}</span>
                          </div>
                        </TableCell>
                        <TableCell className="text-right">{row.current_invitee_count}</TableCell>
                        <TableCell className="text-right" title="users.aff_count，仅作当前字段旁证">{row.rewarded_invite_count}</TableCell>
                        <TableCell className="text-right">{row.window_paying_invitee_count}</TableCell>
                        <TableCell className="text-right font-semibold text-primary">{row.success_topup_count}</TableCell>
                        <TableCell className="text-xs text-muted-foreground">{formatTime(row.last_topup_at)}</TableCell>
                      </TableRow>
                    )
                    if (expandedId !== row.inviter_id) return [main]
                    const detail = (
                      <TableRow key={`${row.inviter_id}-detail`} id={`invite-topup-detail-${row.inviter_id}`} className="bg-muted/20 hover:bg-muted/20">
                        <TableCell></TableCell>
                        <TableCell colSpan={6} className="py-3">
                          {detailState === 'loading' ? (
                            <div className="flex items-center text-sm text-muted-foreground"><Loader2 aria-hidden="true" className="h-4 w-4 mr-2 animate-spin motion-reduce:animate-none" />加载逐笔源记录...</div>
                          ) : detailState === 'unavailable' ? (
                            <div role="alert" className="text-sm text-destructive">逐笔源记录不可用；未将失败显示为空记录。</div>
                          ) : !detailData || detailData.items.length === 0 ? (
                            <div className="text-sm text-muted-foreground">当前父查询下没有逐笔记录</div>
                          ) : (
                            <div>
                              <div className="text-xs text-muted-foreground mb-2">
                                第 {detailData.page}/{detailData.total_pages} 页，共 {detailData.total} 笔；按完成时间、充值 ID 倒序。源金额未对账，不做合计。
                              </div>
                              <Table>
                                <TableHeader>
                                  <TableRow>
                                    <TableHead>充值 ID</TableHead>
                                    <TableHead>被邀请用户</TableHead>
                                    <TableHead className="text-right">源额度</TableHead>
                                    <TableHead className="text-right">源金额</TableHead>
                                    <TableHead>原始状态</TableHead>
                                    <TableHead>完成时间</TableHead>
                                  </TableRow>
                                </TableHeader>
                                <TableBody>
                                  {detailData.items.map(item => (
                                    <TableRow key={item.id}>
                                      <TableCell className="text-xs">{item.id}</TableCell>
                                      <TableCell>{item.username || `#${item.user_id}`}<span className="text-[10px] text-muted-foreground ml-2">ID:{item.user_id}</span></TableCell>
                                      <TableCell className="text-right">{formatRawValue(item.amount)}</TableCell>
                                      <TableCell className="text-right">{formatRawValue(item.money)}</TableCell>
                                      <TableCell className="text-xs">{item.status}</TableCell>
                                      <TableCell className="text-xs text-muted-foreground">{formatTime(item.complete_time)}</TableCell>
                                    </TableRow>
                                  ))}
                                </TableBody>
                              </Table>
                              {detailData.total_pages > 1 && (
                                <div className="mt-2 flex justify-end gap-2">
                                  <Button
                                    variant="outline" size="sm" disabled={detailPage <= 1}
                                    onClick={event => { event.stopPropagation(); void fetchDetails(row.inviter_id, detailPage - 1) }}
                                  ><ChevronLeft aria-hidden="true" className="h-3 w-3 mr-1" />上一页</Button>
                                  <Button
                                    variant="outline" size="sm" disabled={detailPage >= detailData.total_pages}
                                    onClick={event => { event.stopPropagation(); void fetchDetails(row.inviter_id, detailPage + 1) }}
                                  >下一页<ChevronRight aria-hidden="true" className="h-3 w-3 ml-1" /></Button>
                                </div>
                              )}
                            </div>
                          )}
                        </TableCell>
                      </TableRow>
                    )
                    return [main, detail]
                  })}
                </TableBody>
              </Table>
            </div>
          )}

          {listState !== 'loading' && listState !== 'unavailable' && rows.length > 0 && (
            <div className="mt-4 flex flex-col gap-3 text-sm text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
              <div>第 {page} / {totalPages} 页，共 {total} 个邀请人</div>
              <div className="flex flex-wrap items-center gap-2">
                <Button variant="outline" size="sm" onClick={() => changePage(Math.max(1, page - 1))} disabled={page <= 1}>上一页</Button>
                <Button variant="outline" size="sm" onClick={() => changePage(Math.min(totalPages, page + 1))} disabled={page >= totalPages}>下一页</Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
