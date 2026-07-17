import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  AlertTriangle,
  BadgeCheck,
  Ban,
  Calendar,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  CircleDollarSign,
  Clock3,
  Database,
  Eye,
  FileText,
  Filter,
  Loader2,
  Plus,
  ReceiptText,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Upload,
  XCircle,
} from 'lucide-react'
import { useAuth } from '../contexts/AuthContext'
import {
  beginPendingMutation,
  bindOperationReleaseCandidate,
  clearPendingMutation,
  fetchOperationReconciliation,
  getPendingMutation,
  idempotencyHeader,
  listPendingMutations,
  operationReconciliationAction,
  operationReconciliationDecision,
  operationReleaseCandidateMatches,
  type OperationReleaseCandidate,
  type PendingMutationRecord,
} from '../lib/idempotency'
import {
  decodeUtf8Csv,
  effectiveInvoiceCount,
  formatInvoiceTime,
  formatMinorAmount,
  invoiceErrorMessage,
  invoiceFilterParams,
  InvoiceApiError,
  sha256Utf8,
  sourceFreshness,
  validateInvoiceFilters,
  type InvoiceFilters,
  type InvoiceSummaryGroup,
  type MinorAmount,
} from '../lib/invoices'
import { cn } from '../lib/utils'
import { useToast } from './Toast'
import { StatCard } from './StatCard'
import { Badge } from './ui/badge'
import { Button } from './ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from './ui/dialog'
import { Input } from './ui/input'
import { Select } from './ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from './ui/table'

interface InvoiceDocument {
  id: number
  invoice_number: string
  document_kind: 'blue' | 'red'
  related_invoice_number?: string | null
  seller_entity: string
  buyer_name: string
  buyer_tax_id?: string | null
  currency: string
  amount_minor: MinorAmount
  tax_amount_minor: MinorAmount
  minor_unit_scale: number
  status: 'issued' | 'voided' | string
  source: string
  issued_at: string
  voided_at?: string | null
  void_reason?: string | null
  created_by?: string
  created_at: string
  updated_at: string
}

interface InvoiceEvent {
  id?: number
  event_type?: string
  type?: string
  actor?: string
  reason?: string
  created_at?: string
  occurred_at?: string
  details?: Record<string, string | number | boolean | null>
}

interface InvoiceCapabilities {
  view_summary: boolean
  view_list: boolean
  view_detail: boolean
  create: boolean
  import: boolean
  void: boolean
}

interface SummaryData {
  groups: InvoiceSummaryGroup[]
  generated_at: string
  source_health?: { status?: string; message?: string }
  capabilities?: InvoiceCapabilities
}

interface InvoiceListData {
  items: InvoiceDocument[]
  next_cursor?: number | string | null
  has_more: boolean
  capabilities?: InvoiceCapabilities
}

interface InvoiceDetailData {
  document: InvoiceDocument
  events: InvoiceEvent[]
  capabilities?: InvoiceCapabilities
}

interface ImportError {
  code: string
  field?: string
  message: string
}

interface ImportPreviewRow {
  row_number: number
  valid: boolean
  invoice?: Partial<InvoiceDocument>
  errors?: ImportError[]
}

interface ImportPreviewData {
  valid: boolean
  row_count: number
  valid_count: number
  error_count: number
  issue_count: number
  rows: ImportPreviewRow[]
  errors?: ImportError[]
  capabilities?: InvoiceCapabilities
}

interface ApiEnvelope<T> {
  success: boolean
  data?: T
  capabilities?: InvoiceCapabilities
  error?: { code?: string; message?: string }
  message?: string
}

type LoadStatus = 'loading' | 'ready' | 'empty' | 'stale' | 'error'
type InvoicePendingMutation = PendingMutationRecord

const EMPTY_FILTERS: InvoiceFilters = { currency: '', status: '', issuedFrom: '', issuedTo: '' }

const EMPTY_MANUAL_FORM = {
  invoice_number: '',
  document_kind: 'blue' as 'blue' | 'red',
  related_invoice_number: '',
  seller_entity: '',
  buyer_name: '',
  buyer_tax_id: '',
  currency: 'CNY',
  amount_minor: '',
  tax_amount_minor: '0',
  minor_unit_scale: '2',
  issued_at: '',
  reason: '',
}

function statusLabel(status: string): string {
  if (status === 'issued') return '有效'
  if (status === 'voided') return '已作废'
  return status || '未知'
}

function statusVariant(status: string): 'success' | 'destructive' | 'outline' {
  if (status === 'issued') return 'success'
  if (status === 'voided') return 'destructive'
  return 'outline'
}

function maskTaxId(value?: string | null): string {
  if (!value) return '-'
  if (value.length <= 6) return '******'
  return `${value.slice(0, 3)}${'*'.repeat(Math.min(10, value.length - 6))}${value.slice(-3)}`
}

function readCapabilities(body: ApiEnvelope<unknown>): InvoiceCapabilities | null {
  const nested = body.data && typeof body.data === 'object'
    ? (body.data as { capabilities?: unknown }).capabilities
    : undefined
  const value = nested && typeof nested === 'object' ? nested : body.capabilities
  if (!value || typeof value !== 'object') return null
  const candidate = value as Partial<InvoiceCapabilities>
  const keys: (keyof InvoiceCapabilities)[] = ['view_summary', 'view_list', 'view_detail', 'create', 'import', 'void']
  if (!keys.every(key => typeof candidate[key] === 'boolean')) return null
  return candidate as InvoiceCapabilities
}

function normalizeMinorString(value: string, label: string, allowZero: boolean): string {
  if (!/^\d+$/.test(value.trim())) throw new Error(`${label}必须是非负整数（最小货币单位）`)
  const normalized = BigInt(value.trim())
  if (!allowZero && normalized === 0n) throw new Error(`${label}必须大于 0`)
  if (normalized > 9223372036854775807n) throw new Error(`${label}超出服务端整数范围`)
  return normalized.toString()
}

function toShanghaiDateTime(value: string): string {
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(value)) throw new Error('开票时间格式无效')
  return `${value}:00+08:00`
}

export function Invoices() {
  const { token, logout } = useAuth()
  const { showToast } = useToast()
  const apiUrl = import.meta.env.VITE_API_URL || ''

  const [draftFilters, setDraftFilters] = useState<InvoiceFilters>(EMPTY_FILTERS)
  const [filters, setFilters] = useState<InvoiceFilters>(EMPTY_FILTERS)
  const [summary, setSummary] = useState<SummaryData | null>(null)
  const [summaryStatus, setSummaryStatus] = useState<LoadStatus>('loading')
  const [summaryError, setSummaryError] = useState('')
  const [freshnessNow, setFreshnessNow] = useState(() => Date.now())
  const [items, setItems] = useState<InvoiceDocument[]>([])
  const [listStatus, setListStatus] = useState<LoadStatus>('loading')
  const [listError, setListError] = useState('')
  const [nextCursor, setNextCursor] = useState('')
  const [cursor, setCursor] = useState('')
  const [cursorHistory, setCursorHistory] = useState<string[]>([])
  const [refreshing, setRefreshing] = useState(false)
  const [capabilities, setCapabilities] = useState<InvoiceCapabilities | null>(null)

  const [detailOpen, setDetailOpen] = useState(false)
  const [detailLoading, setDetailLoading] = useState(false)
  const [detail, setDetail] = useState<InvoiceDetailData | null>(null)
  const [detailError, setDetailError] = useState('')

  const [manualOpen, setManualOpen] = useState(false)
  const [manualForm, setManualForm] = useState(EMPTY_MANUAL_FORM)
  const [manualSubmitting, setManualSubmitting] = useState(false)

  const [importOpen, setImportOpen] = useState(false)
  const [csvText, setCsvText] = useState('')
  const [importReason, setImportReason] = useState('')
  const [preview, setPreview] = useState<ImportPreviewData | null>(null)
  const [previewHash, setPreviewHash] = useState('')
  const [previewVersion, setPreviewVersion] = useState(-1)
  const [previewing, setPreviewing] = useState(false)
  const [importing, setImporting] = useState(false)

  const [voidOpen, setVoidOpen] = useState(false)
  const [voidReason, setVoidReason] = useState('')
  const [voiding, setVoiding] = useState(false)

  const [pendingMutations, setPendingMutations] = useState<Map<string, InvoicePendingMutation>>(() => {
    const entries = listPendingMutations()
      .filter(item => item.action === 'invoice.create' || item.action === 'invoice.import' || item.action === 'invoice.void')
      .map(item => [item.operationIdentifier, item] as const)
    return new Map(entries)
  })
  const [releaseCandidates, setReleaseCandidates] = useState<Map<string, OperationReleaseCandidate>>(new Map())
  const [reconcilingOperation, setReconcilingOperation] = useState('')

  const summarySequence = useRef(0)
  const listSequence = useRef(0)
  const summaryRef = useRef<SummaryData | null>(null)
  const itemsRef = useRef<InvoiceDocument[]>([])
  const summaryQueryKeyRef = useRef('')
  const listQueryKeyRef = useRef('')
  const detailSequence = useRef(0)
  const detailInvoiceID = useRef<number | null>(null)
  const detailAbortController = useRef<AbortController | null>(null)
  const previewSequence = useRef(0)
  const previewAbortController = useRef<AbortController | null>(null)
  const csvVersion = useRef(0)
  const fileReadSequence = useRef(0)
  const loadSummaryRef = useRef<(silent?: boolean) => Promise<boolean>>(async () => false)
  const loadListRef = useRef<(silent?: boolean) => Promise<boolean>>(async () => false)

  useEffect(() => {
    const timer = window.setInterval(() => setFreshnessNow(Date.now()), 60_000)
    return () => window.clearInterval(timer)
  }, [])

  const summaryFilters = useMemo<InvoiceFilters>(() => ({
    currency: filters.currency,
    status: '',
    issuedFrom: filters.issuedFrom,
    issuedTo: filters.issuedTo,
  }), [filters.currency, filters.issuedFrom, filters.issuedTo])

  const authHeaders = useMemo<Record<string, string>>(() => ({
    'Content-Type': 'application/json',
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
  }), [token])

  const requestJson = useCallback(async <T,>(url: string, options: RequestInit = {}): Promise<{ data: T; body: ApiEnvelope<T> }> => {
    let response: Response
    try {
      response = await fetch(url, { ...options, cache: 'no-store' })
    } catch {
      throw new InvoiceApiError(0, 'NETWORK_ERROR', '网络连接失败，保留上次成功数据')
    }

    let body: ApiEnvelope<T>
    try {
      body = await response.json() as ApiEnvelope<T>
    } catch {
      throw new InvoiceApiError(response.status, 'INVALID_RESPONSE', `服务响应无法解析（HTTP ${response.status}）`)
    }
    if (response.status === 401) {
      logout()
      throw new InvoiceApiError(401, body.error?.code || 'UNAUTHORIZED', invoiceErrorMessage(401))
    }
    if (!response.ok || !body.success || body.data === undefined) {
      const fallback = body.error?.message || body.message
      throw new InvoiceApiError(
        response.status,
        body.error?.code || `HTTP_${response.status}`,
        invoiceErrorMessage(response.status, fallback),
      )
    }
    const discovered = readCapabilities(body as ApiEnvelope<unknown>)
    if (discovered) setCapabilities(discovered)
    return { data: body.data, body }
  }, [logout])

  const rememberPendingMutation = useCallback((pending: InvoicePendingMutation) => {
    setPendingMutations(current => {
      const next = new Map(current)
      next.set(pending.operationIdentifier, pending)
      return next
    })
  }, [])

  const forgetPendingMutation = useCallback((pending: InvoicePendingMutation) => {
    setPendingMutations(current => {
      if (!current.has(pending.operationIdentifier)) return current
      const next = new Map(current)
      next.delete(pending.operationIdentifier)
      return next
    })
  }, [])

  const submitInvoiceMutation = useCallback(async <T,>(input: {
    operationIdentifier: string
    action: 'invoice.create' | 'invoice.import' | 'invoice.void'
    targetType: 'invoice_document' | 'invoice_import'
    targetId: string
    url: string
    body: Record<string, unknown>
  }): Promise<{ data: T; lockCleared: boolean }> => {
    const existing = getPendingMutation(input.operationIdentifier)
    if (existing) {
      rememberPendingMutation(existing)
      throw new Error('该操作已有待对账的幂等所有权，禁止修改内容或生成新请求')
    }

    const bodyJson = JSON.stringify(input.body)
    const fingerprint = `sha256:${await sha256Utf8(bodyJson)}`
    let pending: PendingMutationRecord
    try {
      pending = beginPendingMutation({
        operationIdentifier: input.operationIdentifier,
        fingerprint,
        action: input.action,
        targetType: input.targetType,
        targetId: input.targetId,
        payload: {},
      })
    } catch (error) {
      const stored = getPendingMutation(input.operationIdentifier)
      if (stored) rememberPendingMutation(stored)
      throw error
    }
    rememberPendingMutation(pending)

    try {
      const { data } = await requestJson<T>(input.url, {
        method: 'POST',
        headers: { ...authHeaders, ...idempotencyHeader(pending.key) },
        body: bodyJson,
      })
      const lockCleared = clearPendingMutation(pending)
      if (lockCleared) forgetPendingMutation(pending)
      else rememberPendingMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
      return { data, lockCleared }
    } catch (error) {
      const apiError = error instanceof InvoiceApiError ? error : null
      const uncertain = !apiError || apiError.status === 0 || apiError.status === 408 || apiError.status >= 500 || apiError.code === 'INVALID_RESPONSE'
      if (!uncertain) {
        if (clearPendingMutation(pending)) forgetPendingMutation(pending)
        else rememberPendingMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
        throw error
      }
      rememberPendingMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
      const message = error instanceof Error ? error.message : '提交结果未知'
      throw new Error(`${message}；幂等所有权已锁定，请查询 Tool Store 终态，切勿修改后重试`)
    }
  }, [authHeaders, forgetPendingMutation, rememberPendingMutation, requestJson])

  const reconcilePendingMutation = useCallback(async (pending: InvoicePendingMutation) => {
    if (reconcilingOperation) return
    setReconcilingOperation(pending.operationIdentifier)
    try {
      const candidate = releaseCandidates.get(pending.operationIdentifier)
      const candidateMatches = operationReleaseCandidateMatches(candidate, pending)
      const reconciliation = candidateMatches
        ? candidate
        : await fetchOperationReconciliation(apiUrl, authHeaders, pending)
      const decision = operationReconciliationDecision(reconciliation.status)
      const nextAction = operationReconciliationAction(reconciliation.status, candidateMatches)
      if (nextAction === 'keep_locked') {
        setReleaseCandidates(current => {
          const next = new Map(current)
          next.delete(pending.operationIdentifier)
          return next
        })
        const message = reconciliation.status === 'not_found'
          ? 'Tool Store 尚未发现可证明终态的审计记录，操作继续锁定'
          : reconciliation.status === 'pending'
            ? `审计 #${reconciliation.audit_id} 尚未形成终态，操作继续锁定`
            : `审计 #${reconciliation.audit_id} 的结果仍不确定，请人工处置`
        showToast('error', message)
        return
      }
      if (nextAction === 'confirm_release') {
        setReleaseCandidates(current => {
          const next = new Map(current)
          next.set(pending.operationIdentifier, bindOperationReleaseCandidate(pending, reconciliation))
          return next
        })
        showToast('info', `审计 #${reconciliation.audit_id} 确认为 ${reconciliation.status}；请再次确认解除本地锁`)
        return
      }
      if (!clearPendingMutation(pending)) {
        rememberPendingMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
        showToast('error', '审计已确认终态，但浏览器未能安全清理本地锁，请再次对账')
        return
      }
      forgetPendingMutation(pending)
      if (pending.action === 'invoice.create') {
        setManualOpen(false)
        setManualForm(EMPTY_MANUAL_FORM)
      } else if (pending.action === 'invoice.import') {
        previewSequence.current += 1
        previewAbortController.current?.abort()
        csvVersion.current += 1
        setImportOpen(false)
        setCsvText('')
        setImportReason('')
        setPreview(null)
        setPreviewHash('')
        setPreviewVersion(-1)
        setPreviewing(false)
      } else {
        setVoidOpen(false)
        setVoidReason('')
      }
      setReleaseCandidates(current => {
        const next = new Map(current)
        next.delete(pending.operationIdentifier)
        return next
      })
      showToast(decision === 'applied' ? 'success' : 'info', decision === 'applied'
        ? `审计 #${reconciliation.audit_id} 确认操作已生效，锁已解除`
        : `审计 #${reconciliation.audit_id} 确认操作未生效，锁已释放`)
      void Promise.all([loadSummaryRef.current(true), loadListRef.current(true)])
    } catch (error) {
      showToast('error', error instanceof Error ? error.message : '对账失败，操作继续锁定')
    } finally {
      setReconcilingOperation('')
    }
  }, [apiUrl, authHeaders, forgetPendingMutation, reconcilingOperation, releaseCandidates, rememberPendingMutation, showToast])

  const loadSummary = useCallback(async (silent = false): Promise<boolean> => {
    const sequence = ++summarySequence.current
    const params = invoiceFilterParams(summaryFilters, false)
    const queryKey = params.toString()
    const sameQuery = summaryQueryKeyRef.current === queryKey
    if (!sameQuery) {
      summaryQueryKeyRef.current = queryKey
      summaryRef.current = null
      setSummary(null)
    }
    if (!silent || !sameQuery) setSummaryStatus('loading')
    setSummaryError('')
    try {
      const { data } = await requestJson<SummaryData>(`${apiUrl}/api/invoices/summary?${params}`, { headers: authHeaders })
      if (sequence !== summarySequence.current || summaryQueryKeyRef.current !== queryKey) return false
      const groups = Array.isArray(data.groups) ? data.groups : []
      const normalized = { ...data, groups }
      setSummary(normalized)
      setFreshnessNow(Date.now())
      summaryRef.current = normalized
      setSummaryStatus(groups.length === 0 ? 'empty' : sourceFreshness(data.generated_at) === 'stale' ? 'stale' : 'ready')
      return true
    } catch (error) {
      if (sequence !== summarySequence.current || summaryQueryKeyRef.current !== queryKey) return false
      const message = error instanceof Error ? error.message : '统计加载失败'
      setSummaryError(message)
      setSummaryStatus(summaryRef.current ? 'stale' : 'error')
      return false
    }
  }, [apiUrl, authHeaders, requestJson, summaryFilters])

  const loadList = useCallback(async (silent = false): Promise<boolean> => {
    const sequence = ++listSequence.current
    const params = invoiceFilterParams(filters)
    params.set('limit', '30')
    if (cursor) params.set('cursor', cursor)
    const queryKey = params.toString()
    const sameQuery = listQueryKeyRef.current === queryKey
    if (!sameQuery) {
      listQueryKeyRef.current = queryKey
      itemsRef.current = []
      setItems([])
      setNextCursor('')
    }
    if (!silent || !sameQuery) setListStatus('loading')
    setListError('')
    try {
      const { data } = await requestJson<InvoiceListData>(`${apiUrl}/api/invoices?${params}`, { headers: authHeaders })
      if (sequence !== listSequence.current || listQueryKeyRef.current !== queryKey) return false
      const rows = Array.isArray(data.items) ? data.items : []
      setItems(rows)
      itemsRef.current = rows
      setNextCursor(data.has_more && data.next_cursor !== null && data.next_cursor !== undefined ? String(data.next_cursor) : '')
      setListStatus(rows.length === 0 ? 'empty' : 'ready')
      return true
    } catch (error) {
      if (sequence !== listSequence.current || listQueryKeyRef.current !== queryKey) return false
      const message = error instanceof Error ? error.message : '发票列表加载失败'
      setListError(message)
      setListStatus(itemsRef.current.length > 0 ? 'stale' : 'error')
      return false
    }
  }, [apiUrl, authHeaders, cursor, filters, requestJson])

  useEffect(() => { loadSummaryRef.current = loadSummary }, [loadSummary])
  useEffect(() => { loadListRef.current = loadList }, [loadList])

  useEffect(() => { void loadSummary() }, [loadSummary])
  useEffect(() => { void loadList() }, [loadList])
  useEffect(() => () => {
    summarySequence.current += 1
    listSequence.current += 1
    detailSequence.current += 1
    previewSequence.current += 1
    detailAbortController.current?.abort()
    previewAbortController.current?.abort()
  }, [])

  const refreshAll = async () => {
    if (refreshing) return
    setRefreshing(true)
    const results = await Promise.all([loadSummary(true), loadList(true)])
    setRefreshing(false)
    const successCount = results.filter(Boolean).length
    if (successCount === 2) showToast('success', '发票数据已刷新')
    else if (successCount === 1) showToast('info', '部分数据刷新失败，失败区域保留上次结果')
    else showToast('error', '刷新失败，未把不可用数据展示为 0')
  }

  const applyFilters = () => {
    const validation = validateInvoiceFilters(draftFilters)
    if (validation) {
      showToast('error', validation)
      return
    }
    setCursor('')
    setCursorHistory([])
    setFilters({ ...draftFilters, currency: draftFilters.currency.trim().toUpperCase() })
  }

  const resetFilters = () => {
    setDraftFilters(EMPTY_FILTERS)
    setFilters(EMPTY_FILTERS)
    setCursor('')
    setCursorHistory([])
  }

  const openDetail = async (row: InvoiceDocument) => {
    detailAbortController.current?.abort()
    const controller = new AbortController()
    detailAbortController.current = controller
    const sequence = ++detailSequence.current
    detailInvoiceID.current = row.id
    setDetailOpen(true)
    setDetailLoading(true)
    setDetailError('')
    setDetail({ document: row, events: [] })
    try {
      const { data } = await requestJson<InvoiceDetailData>(`${apiUrl}/api/invoices/${encodeURIComponent(String(row.id))}`, { headers: authHeaders, signal: controller.signal })
      if (sequence !== detailSequence.current || detailInvoiceID.current !== row.id || controller.signal.aborted) return
      if (data.document?.id !== row.id) throw new Error('详情响应与所选发票不匹配')
      setDetail({ ...data, events: Array.isArray(data.events) ? data.events : [] })
    } catch (error) {
      if (controller.signal.aborted || sequence !== detailSequence.current) return
      setDetailError(error instanceof Error ? error.message : '详情加载失败')
    } finally {
      if (sequence === detailSequence.current && !controller.signal.aborted) setDetailLoading(false)
    }
  }

  const closeDetail = () => {
    detailSequence.current += 1
    detailInvoiceID.current = null
    detailAbortController.current?.abort()
    detailAbortController.current = null
    setDetailOpen(false)
    setDetailLoading(false)
    setDetail(null)
    setDetailError('')
  }

  const submitManual = async () => {
    if (manualSubmitting) return
    setManualSubmitting(true)
    try {
      if (!manualForm.invoice_number.trim() || !manualForm.seller_entity.trim() || !manualForm.buyer_name.trim()) {
        throw new Error('发票号、销售方主体和购方名称为必填项')
      }
      if (!manualForm.issued_at) throw new Error('请选择开票时间')
      if (!manualForm.reason.trim()) throw new Error('登记原因不能为空')
      if (manualForm.document_kind === 'red' && !manualForm.related_invoice_number.trim()) throw new Error('红字发票必须填写关联原票号')
      if (manualForm.document_kind === 'blue' && manualForm.related_invoice_number.trim()) throw new Error('蓝票不能填写关联原票号')
      if (manualForm.related_invoice_number.trim() === manualForm.invoice_number.trim()) throw new Error('关联原票号不能与当前发票号相同')
      if (!/^[A-Za-z]{3}$/.test(manualForm.currency.trim())) throw new Error('币种必须是 3 位字母代码，例如 CNY')
      const scale = Number(manualForm.minor_unit_scale)
      if (!Number.isInteger(scale) || scale < 0 || scale > 9) throw new Error('小数位必须是 0–9 的整数')
      const amountMinor = normalizeMinorString(manualForm.amount_minor, '含税金额', false)
      const taxAmountMinor = normalizeMinorString(manualForm.tax_amount_minor, '税额', true)
      if (BigInt(taxAmountMinor) > BigInt(amountMinor)) throw new Error('税额不能大于含税金额')
      const payload = {
        invoice_number: manualForm.invoice_number.trim(),
        document_kind: manualForm.document_kind,
        related_invoice_number: manualForm.document_kind === 'red' ? manualForm.related_invoice_number.trim() : undefined,
        seller_entity: manualForm.seller_entity.trim(),
        buyer_name: manualForm.buyer_name.trim(),
        buyer_tax_id: manualForm.buyer_tax_id.trim() || undefined,
        currency: manualForm.currency.trim().toUpperCase(),
        amount_minor: amountMinor,
        tax_amount_minor: taxAmountMinor,
        minor_unit_scale: scale,
        issued_at: toShanghaiDateTime(manualForm.issued_at),
        reason: manualForm.reason.trim(),
      }
      const result = await submitInvoiceMutation<{ document: InvoiceDocument; capabilities?: InvoiceCapabilities }>({
        operationIdentifier: 'invoice.create',
        action: 'invoice.create',
        targetType: 'invoice_document',
        targetId: payload.invoice_number,
        url: `${apiUrl}/api/invoices`,
        body: payload,
      })
      setManualOpen(false)
      setManualForm(EMPTY_MANUAL_FORM)
      showToast(result.lockCleared ? 'success' : 'error', result.lockCleared
        ? '发票已登记并写入审计链'
        : '发票已登记，但本地操作锁未能清理，请查询终态')
      await refreshAll()
    } catch (error) {
      showToast('error', error instanceof Error ? error.message : '登记失败')
    } finally {
      setManualSubmitting(false)
    }
  }

  const updateCsvText = (value: string) => {
    fileReadSequence.current += 1
    previewSequence.current += 1
    previewAbortController.current?.abort()
    previewAbortController.current = null
    csvVersion.current += 1
    setCsvText(value)
    setPreview(null)
    setPreviewing(false)
    setPreviewHash('')
    setPreviewVersion(-1)
  }

  const readCsvFile = async (file: File) => {
    const sequence = ++fileReadSequence.current
    if (file.size > 1024 * 1024) {
      showToast('error', 'CSV 文件超过 1 MiB 限制')
      return
    }
    try {
      const buffer = await file.arrayBuffer()
      if (sequence !== fileReadSequence.current) return
      updateCsvText(decodeUtf8Csv(buffer))
    } catch (error) {
      showToast('error', error instanceof TypeError ? 'CSV 不是有效的 UTF-8 编码' : '无法读取 CSV 文件')
    }
  }

  const previewCsv = async () => {
    if (previewing) return
    if (!csvText.trim()) {
      showToast('error', '请先选择或粘贴 CSV 内容')
      return
    }
    previewAbortController.current?.abort()
    const controller = new AbortController()
    previewAbortController.current = controller
    const sequence = ++previewSequence.current
    const version = csvVersion.current
    const contents = csvText
    setPreviewing(true)
    setPreview(null)
    setPreviewHash('')
    setPreviewVersion(-1)
    try {
      const contentHash = await sha256Utf8(contents)
      if (sequence !== previewSequence.current || version !== csvVersion.current || controller.signal.aborted) return
      const { data } = await requestJson<{ preview: ImportPreviewData; capabilities?: InvoiceCapabilities }>(`${apiUrl}/api/invoices/import/preview`, {
        method: 'POST', headers: authHeaders, body: JSON.stringify({ csv: contents }), signal: controller.signal,
      })
      if (sequence !== previewSequence.current || version !== csvVersion.current || controller.signal.aborted) return
      setPreview(data.preview)
      setPreviewHash(contentHash)
      setPreviewVersion(version)
      if (data.preview.error_count > 0) showToast('info', `预览完成：${data.preview.error_count} 行需要修正`)
      else showToast('success', `预览完成：${data.preview.valid_count} 行可导入`)
    } catch (error) {
      if (controller.signal.aborted || sequence !== previewSequence.current) return
      showToast('error', error instanceof Error ? error.message : 'CSV 预览失败')
    } finally {
      if (sequence === previewSequence.current) setPreviewing(false)
    }
  }

  const confirmImport = async () => {
    if (importing || !preview?.valid || preview.valid_count < 1) return
    if (!importReason.trim()) {
      showToast('error', '导入原因不能为空')
      return
    }
    setImporting(true)
    try {
      const currentHash = await sha256Utf8(csvText)
      const completelyValid = preview.valid && preview.error_count === 0 && (preview.issue_count ?? 0) === 0 && (preview.errors?.length ?? 0) === 0
      if (!completelyValid || !previewHash || previewVersion !== csvVersion.current || currentHash !== previewHash) {
        setPreview(null)
        setPreviewHash('')
        setPreviewVersion(-1)
        throw new Error('CSV 内容已变化或预览并非完全有效，请重新预览')
      }
      const payload = { csv: csvText, reason: importReason.trim() }
      const result = await submitInvoiceMutation<{ items: InvoiceDocument[]; count: number; capabilities?: InvoiceCapabilities }>({
        operationIdentifier: 'invoice.import',
        action: 'invoice.import',
        targetType: 'invoice_import',
        targetId: 'batch',
        url: `${apiUrl}/api/invoices/import/confirm`,
        body: payload,
      })
      setImportOpen(false)
      updateCsvText('')
      setImportReason('')
      showToast(result.lockCleared ? 'success' : 'error', result.lockCleared
        ? `已导入 ${result.data.count} 张发票`
        : `已导入 ${result.data.count} 张发票，但本地操作锁未清理`)
      await refreshAll()
    } catch (error) {
      showToast('error', error instanceof Error ? error.message : 'CSV 导入失败')
    } finally {
      setImporting(false)
    }
  }

  const confirmVoid = async () => {
    if (voiding || !detail?.document) return
    if (!voidReason.trim()) {
      showToast('error', '作废原因不能为空')
      return
    }
    setVoiding(true)
    try {
      const invoiceID = detail.document.id
      const payload = { reason: voidReason.trim() }
      const result = await submitInvoiceMutation<{ document: InvoiceDocument; capabilities?: InvoiceCapabilities }>({
        operationIdentifier: `invoice.void:${invoiceID}`,
        action: 'invoice.void',
        targetType: 'invoice_document',
        targetId: String(invoiceID),
        url: `${apiUrl}/api/invoices/${encodeURIComponent(String(invoiceID))}/void`,
        body: payload,
      })
      setDetail(previous => previous?.document.id === invoiceID ? { ...previous, document: result.data.document } : previous)
      setVoidOpen(false)
      setVoidReason('')
      showToast(result.lockCleared ? 'success' : 'error', result.lockCleared
        ? '发票已作废，原记录和事件历史已保留'
        : '发票已作废，但本地操作锁未清理，请查询终态')
      await refreshAll()
    } catch (error) {
      showToast('error', error instanceof Error ? error.message : '作废失败')
    } finally {
      setVoiding(false)
    }
  }

  const can = (capability: keyof InvoiceCapabilities) => capabilities?.[capability] === true
  const generatedFreshness = sourceFreshness(summary?.generated_at, freshnessNow)
  const pendingList = Array.from(pendingMutations.values())
  const pendingCreate = pendingMutations.get('invoice.create') ?? null
  const pendingImport = pendingMutations.get('invoice.import') ?? null
  const pendingVoid = detail?.document ? pendingMutations.get(`invoice.void:${detail.document.id}`) ?? null : null
  const previewMatchesCurrentCsv = Boolean(previewHash) && previewVersion === csvVersion.current
  const previewCompletelyValid = Boolean(preview?.valid)
    && (preview?.valid_count ?? 0) > 0
    && (preview?.error_count ?? 0) === 0
    && (preview?.issue_count ?? 0) === 0
    && (preview?.errors?.length ?? 0) === 0

  const pendingActionLabel = (pending: InvoicePendingMutation) => {
    if (pending.action === 'invoice.create') return `登记发票 ${pending.targetId}`
    if (pending.action === 'invoice.import') return '批量导入发票'
    return `作废发票 #${pending.targetId}`
  }

  return (
    <div className="space-y-6 animate-in fade-in duration-500">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <h2 className="text-3xl font-bold tracking-tight">发票统计</h2>
          <p className="mt-1 text-muted-foreground">独立台账记录已开票金额、作废状态与审计事件</p>
          <p className="mt-1 text-xs text-muted-foreground">登记、导入和作废理由会进入审计，请勿填写购方姓名、税号等 PII。</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={refreshAll} disabled={refreshing} aria-label="刷新发票统计和明细">
            <RefreshCw className={cn('mr-2 h-4 w-4', refreshing && 'animate-spin')} />刷新
          </Button>
          {can('create') && (
            <Button variant="outline" size="sm" onClick={() => setManualOpen(true)} disabled={Boolean(pendingCreate)} title={pendingCreate ? '存在待对账登记操作' : undefined}>
              <Plus className="mr-2 h-4 w-4" />手工登记
            </Button>
          )}
          {can('import') && (
            <Button size="sm" onClick={() => setImportOpen(true)} disabled={Boolean(pendingImport)} title={pendingImport ? '存在待对账导入操作' : undefined}>
              <Upload className="mr-2 h-4 w-4" />CSV 导入
            </Button>
          )}
        </div>
      </div>

      <Card role="status" aria-live="polite" className={cn(
        'border-dashed',
        (summaryStatus === 'stale' || summaryStatus === 'error') && 'border-amber-500/60 bg-amber-500/5',
      )}>
        <CardContent className="flex flex-col gap-3 p-4 text-sm sm:flex-row sm:items-center sm:justify-between">
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2">
            <span className="flex items-center gap-2 font-medium">
              <Database className="h-4 w-4" />数据源
              {summaryStatus === 'error' ? <Badge variant="destructive">不可用</Badge>
                : summaryStatus === 'stale' || generatedFreshness === 'stale' ? <Badge variant="warning">数据已过期</Badge>
                  : summaryStatus === 'loading' ? <Badge variant="outline">连接中</Badge>
                    : <Badge variant="success">正常</Badge>}
            </span>
            <span className="flex items-center gap-2 text-muted-foreground">
              <Clock3 className="h-4 w-4" />数据时间：{summary?.generated_at ? formatInvoiceTime(summary.generated_at) : '尚无成功快照'}
            </span>
          </div>
          <span className="text-xs text-muted-foreground">
            {capabilities === null ? '权限尚未确认，高风险操作默认隐藏' : '权限能力已由服务端确认'}
          </span>
        </CardContent>
      </Card>

      {pendingList.length > 0 && (
        <Card className="border-amber-500/60 bg-amber-500/5" role="status" aria-live="polite">
          <CardHeader className="pb-3"><CardTitle className="flex items-center gap-2 text-base"><AlertTriangle className="h-4 w-4 text-amber-600" />待对账操作</CardTitle><CardDescription>以下写操作结果尚未安全确认。对应表单保持锁定，禁止修改内容后重新提交。</CardDescription></CardHeader>
          <CardContent className="space-y-2">
            {pendingList.map(pending => {
              const candidate = releaseCandidates.get(pending.operationIdentifier)
              const confirmingRelease = operationReleaseCandidateMatches(candidate, pending)
              const reconciling = reconcilingOperation === pending.operationIdentifier
              return <div key={pending.operationIdentifier} className="flex flex-col gap-3 rounded-lg border bg-background/70 p-3 sm:flex-row sm:items-center sm:justify-between"><div><p className="font-medium">{pendingActionLabel(pending)}</p><p className="font-mono text-xs text-muted-foreground">幂等键 {pending.key}</p></div><Button variant={confirmingRelease ? 'destructive' : 'outline'} size="sm" disabled={Boolean(reconcilingOperation)} onClick={() => void reconcilePendingMutation(pending)} aria-label={`${confirmingRelease ? '确认解除' : '查询'} ${pendingActionLabel(pending)} 的操作锁`}>{reconciling ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <ShieldCheck className="mr-2 h-4 w-4" />}{confirmingRelease ? '确认解除本地锁' : '查询 Tool Store 终态'}</Button></div>
            })}
          </CardContent>
        </Card>
      )}

      {summaryError && (
        <div role="alert" className="flex items-start gap-2 rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-800 dark:text-amber-200">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{summaryError}{summary ? '；以下为上次成功数据，不代表实时值。' : '；未将失败结果显示为 0。'}</span>
        </div>
      )}

      {summaryStatus === 'loading' ? (
        <div className="flex min-h-48 items-center justify-center" role="status" aria-live="polite"><Loader2 className="h-8 w-8 animate-spin text-primary" /><span className="sr-only">正在加载发票统计</span></div>
      ) : summaryStatus === 'error' && !summary ? (
        <Card><CardContent className="py-14 text-center"><XCircle className="mx-auto mb-3 h-9 w-9 text-destructive" /><p className="font-medium">统计暂不可用</p><p className="mt-1 text-sm text-muted-foreground">修复数据源后刷新，不会把失败误报成零开票。</p></CardContent></Card>
      ) : summary?.groups.length === 0 ? (
        <Card><CardContent className="py-14 text-center"><ReceiptText className="mx-auto mb-3 h-9 w-9 text-muted-foreground" /><p className="font-medium">暂无发票统计</p><p className="mt-1 text-sm text-muted-foreground">数据源可用，但当前筛选范围没有发票记录。</p></CardContent></Card>
      ) : (
        <div className="space-y-5">
          {summary?.groups.map(group => (
            <section key={`${group.currency}-${group.minor_unit_scale}`} className="space-y-3">
              <div className="flex items-center gap-2">
                <Badge variant="outline" className="font-mono">{group.currency}</Badge>
                <span className="text-xs text-muted-foreground">所有金额均来自后端最小货币单位，未在浏览器浮点汇总</span>
              </div>
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
                <StatCard title="蓝票开具金额" value={formatMinorAmount(group.blue_issued_minor, group.currency, group.minor_unit_scale)} subValue="当前有效蓝票金额" icon={FileText} color="blue" />
                <StatCard title="红字发票金额" value={formatMinorAmount(group.red_issued_minor, group.currency, group.minor_unit_scale)} subValue="当前有效红票冲减金额" icon={RotateCcw} color="rose" />
                <StatCard title="作废金额" value={formatMinorAmount(group.voided_minor, group.currency, group.minor_unit_scale)} subValue={`${group.voided_count} 张已作废`} icon={Ban} color="orange" />
                <StatCard title="净已开票金额" value={formatMinorAmount(group.net_issued_minor, group.currency, group.minor_unit_scale)} subValue="蓝票扣除作废/冲减后的后端口径" icon={CircleDollarSign} color="emerald" />
                <StatCard title="有效发票张数" value={effectiveInvoiceCount(group) === null ? '未提供' : `${effectiveInvoiceCount(group)} 张`} subValue="不包含已作废记录" icon={BadgeCheck} color="green" />
                <StatCard title="异常记录" value={group.anomaly_count === undefined ? '未提供' : `${group.anomaly_count} 条`} subValue={group.anomaly_count === undefined ? '等待对账模块提供异常口径' : '需要人工核对'} icon={AlertTriangle} color="amber" />
              </div>
            </section>
          ))}
        </div>
      )}

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base"><Filter className="h-4 w-4" />筛选条件</CardTitle>
          <CardDescription>日期同时过滤汇总和明细；状态仅过滤下方明细，不改变汇总 KPI。</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label className="space-y-1 text-xs font-medium text-muted-foreground">币种
              <Input value={draftFilters.currency} onChange={event => setDraftFilters(current => ({ ...current, currency: event.target.value }))} placeholder="全部，如 CNY" className="mt-1 uppercase" maxLength={3} />
            </label>
            <label className="space-y-1 text-xs font-medium text-muted-foreground">状态（仅明细）
              <Select value={draftFilters.status} onChange={event => setDraftFilters(current => ({ ...current, status: event.target.value as InvoiceFilters['status'] }))} className="mt-1">
                <option value="">全部状态</option><option value="issued">有效</option><option value="voided">已作废</option>
              </Select>
            </label>
            <label className="space-y-1 text-xs font-medium text-muted-foreground">开始日期
              <div className="relative mt-1"><Calendar className="absolute left-2.5 top-2.5 h-4 w-4" /><Input type="date" value={draftFilters.issuedFrom} onChange={event => setDraftFilters(current => ({ ...current, issuedFrom: event.target.value }))} className="pl-9" /></div>
            </label>
            <label className="space-y-1 text-xs font-medium text-muted-foreground">结束日期
              <div className="relative mt-1"><Calendar className="absolute left-2.5 top-2.5 h-4 w-4" /><Input type="date" value={draftFilters.issuedTo} onChange={event => setDraftFilters(current => ({ ...current, issuedTo: event.target.value }))} className="pl-9" /></div>
            </label>
          </div>
          <div className="mt-4 flex justify-end gap-2"><Button variant="ghost" size="sm" onClick={resetFilters}>重置</Button><Button size="sm" onClick={applyFilters}>应用筛选</Button></div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader><CardTitle className="text-base">发票明细</CardTitle><CardDescription>点击详情查看购方快照、金额和不可变事件。</CardDescription></CardHeader>
        <CardContent className="p-0">
          {listError && <div role="alert" className="mx-4 mb-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm">{listError}{items.length > 0 ? '；当前保留上次成功列表。' : '；未将失败显示为空列表。'}</div>}
          {listStatus === 'loading' ? <div className="flex justify-center py-16" role="status" aria-live="polite"><Loader2 className="h-8 w-8 animate-spin text-primary" /><span className="sr-only">正在加载发票明细</span></div>
            : listStatus === 'error' && items.length === 0 ? <div className="py-16 text-center"><XCircle className="mx-auto mb-3 h-8 w-8 text-destructive" /><p className="font-medium">明细加载失败</p></div>
              : items.length === 0 ? <div className="py-16 text-center"><ReceiptText className="mx-auto mb-3 h-8 w-8 text-muted-foreground" /><p className="font-medium">暂无符合条件的发票</p><p className="mt-1 text-sm text-muted-foreground">这是成功查询后的空结果。</p></div>
                : <div className="overflow-x-auto"><Table><TableHeader className="bg-muted/50"><TableRow><TableHead>发票号</TableHead><TableHead>类型</TableHead><TableHead>购方</TableHead><TableHead>币种</TableHead><TableHead className="text-right">含税金额</TableHead><TableHead>状态</TableHead><TableHead>开票时间</TableHead><TableHead className="text-right">操作</TableHead></TableRow></TableHeader><TableBody>
                  {items.map(row => <TableRow key={row.id}><TableCell className="font-mono text-xs">{row.invoice_number}</TableCell><TableCell><Badge variant={row.document_kind === 'red' ? 'destructive' : 'outline'}>{row.document_kind === 'red' ? '红票' : '蓝票'}</Badge></TableCell><TableCell><div className="font-medium">{row.buyer_name}</div><div className="text-xs text-muted-foreground">{maskTaxId(row.buyer_tax_id)}</div></TableCell><TableCell><Badge variant="outline">{row.currency}</Badge></TableCell><TableCell className="text-right font-mono font-medium">{formatMinorAmount(row.amount_minor, row.currency, row.minor_unit_scale)}</TableCell><TableCell><Badge variant={statusVariant(row.status)}>{statusLabel(row.status)}</Badge></TableCell><TableCell className="whitespace-nowrap text-sm text-muted-foreground">{formatInvoiceTime(row.issued_at)}</TableCell><TableCell className="text-right">{can('view_detail') ? <Button variant="ghost" size="sm" onClick={() => void openDetail(row)}><Eye className="mr-2 h-4 w-4" />详情</Button> : <span className="text-xs text-muted-foreground">无详情权限</span>}</TableCell></TableRow>)}
                </TableBody></Table></div>}
          <div className="flex items-center justify-between border-t px-4 py-3"><span className="text-xs text-muted-foreground">第 {cursorHistory.length + 1} 页 · 本页 {items.length} 条</span><div className="flex gap-2"><Button variant="outline" size="sm" disabled={cursorHistory.length === 0 || listStatus === 'loading'} onClick={() => { const history = [...cursorHistory]; const previous = history.pop() ?? ''; setCursorHistory(history); setCursor(previous) }}><ChevronLeft className="mr-1 h-4 w-4" />上一页</Button><Button variant="outline" size="sm" disabled={!nextCursor || listStatus === 'loading'} onClick={() => { setCursorHistory(history => [...history, cursor]); setCursor(nextCursor) }}>下一页<ChevronRight className="ml-1 h-4 w-4" /></Button></div></div>
        </CardContent>
      </Card>

      <Dialog open={detailOpen} onOpenChange={open => { if (!open) closeDetail() }}><DialogContent className="max-w-3xl"><DialogHeader><DialogTitle>发票详情</DialogTitle><DialogDescription>敏感税号默认脱敏，完整值不写入前端日志。</DialogDescription></DialogHeader>
        {detailLoading && <div className="flex justify-center py-10" role="status" aria-live="polite"><Loader2 className="h-7 w-7 animate-spin" /><span className="sr-only">正在加载发票详情</span></div>}
        {detailError && <div role="alert" className="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">{detailError}</div>}
        {detail?.document && <div className="space-y-5"><div className="grid grid-cols-1 gap-3 rounded-xl border bg-muted/20 p-4 sm:grid-cols-2"><div><p className="text-xs text-muted-foreground">发票号</p><p className="font-mono font-medium">{detail.document.invoice_number}</p></div><div><p className="text-xs text-muted-foreground">类型 / 状态</p><div className="flex gap-2"><Badge variant={detail.document.document_kind === 'red' ? 'destructive' : 'outline'}>{detail.document.document_kind === 'red' ? '红票' : '蓝票'}</Badge><Badge variant={statusVariant(detail.document.status)}>{statusLabel(detail.document.status)}</Badge></div></div>{detail.document.related_invoice_number && <div className="sm:col-span-2"><p className="text-xs text-muted-foreground">关联原票号</p><p className="font-mono">{detail.document.related_invoice_number}</p></div>}<div><p className="text-xs text-muted-foreground">销售方</p><p>{detail.document.seller_entity}</p></div><div><p className="text-xs text-muted-foreground">购方</p><p>{detail.document.buyer_name}</p><p className="font-mono text-xs text-muted-foreground">{maskTaxId(detail.document.buyer_tax_id)}</p></div><div><p className="text-xs text-muted-foreground">含税金额</p><p className="font-mono font-semibold">{formatMinorAmount(detail.document.amount_minor, detail.document.currency, detail.document.minor_unit_scale)}</p></div><div><p className="text-xs text-muted-foreground">税额</p><p className="font-mono">{formatMinorAmount(detail.document.tax_amount_minor, detail.document.currency, detail.document.minor_unit_scale)}</p></div><div><p className="text-xs text-muted-foreground">开票时间</p><p>{formatInvoiceTime(detail.document.issued_at)}</p></div><div><p className="text-xs text-muted-foreground">来源</p><p>{detail.document.source || '-'}</p></div>{detail.document.void_reason && <div className="sm:col-span-2"><p className="text-xs text-muted-foreground">作废原因</p><p className="text-destructive">{detail.document.void_reason}</p></div>}</div>
          <div><h3 className="mb-2 text-sm font-semibold">事件记录</h3>{detail.events.length === 0 ? <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">暂无事件或事件仍在加载。</p> : <div className="space-y-2">{detail.events.map((event, index) => <div key={event.id ?? index} className="flex items-start justify-between gap-4 rounded-lg border p-3 text-sm"><div><p className="font-medium">{event.event_type || event.type || '事件'}</p><p className="text-muted-foreground">{event.reason || '无附加原因'}{event.actor ? ` · ${event.actor}` : ''}</p>{event.details && <div className="mt-2 flex flex-wrap gap-1">{Object.entries(event.details).map(([key, value]) => <Badge key={key} variant="outline" className="font-normal">{key}: {String(value)}</Badge>)}</div>}</div><span className="whitespace-nowrap text-xs text-muted-foreground">{formatInvoiceTime(event.occurred_at || event.created_at)}</span></div>)}</div>}</div>
          {detail.document.status === 'issued' && can('void') && <div className="flex justify-end"><Button variant="destructive" disabled={Boolean(pendingVoid)} onClick={() => setVoidOpen(true)}><Ban className="mr-2 h-4 w-4" />{pendingVoid ? '作废操作待对账' : '作废发票'}</Button></div>}
        </div>}
      </DialogContent></Dialog>

      <Dialog open={manualOpen} onOpenChange={open => { setManualOpen(open); if (!open && !pendingCreate) setManualForm(EMPTY_MANUAL_FORM) }}><DialogContent className="max-w-2xl"><DialogHeader><DialogTitle>手工登记发票</DialogTitle><DialogDescription>金额以最小货币单位录入，例如 CNY 123.45 元填 12345；提交由服务端再次校验。</DialogDescription></DialogHeader><fieldset disabled={Boolean(pendingCreate) || manualSubmitting} className="grid grid-cols-1 gap-4 disabled:opacity-70 sm:grid-cols-2">
        <label className="text-sm">发票号<Input className="mt-1" value={manualForm.invoice_number} onChange={event => setManualForm(form => ({ ...form, invoice_number: event.target.value }))} /></label><label className="text-sm">发票类型<Select className="mt-1" value={manualForm.document_kind} onChange={event => setManualForm(form => ({ ...form, document_kind: event.target.value as 'blue' | 'red', related_invoice_number: '' }))}><option value="blue">蓝票</option><option value="red">红字发票</option></Select></label>{manualForm.document_kind === 'red' && <label className="text-sm sm:col-span-2">关联原票号<Input className="mt-1 font-mono" value={manualForm.related_invoice_number} onChange={event => setManualForm(form => ({ ...form, related_invoice_number: event.target.value }))} /></label>}<label className="text-sm">销售方主体<Input className="mt-1" value={manualForm.seller_entity} onChange={event => setManualForm(form => ({ ...form, seller_entity: event.target.value }))} /></label><label className="text-sm">购方名称<Input className="mt-1" value={manualForm.buyer_name} onChange={event => setManualForm(form => ({ ...form, buyer_name: event.target.value }))} /></label><label className="text-sm">购方税号（可选）<Input className="mt-1" value={manualForm.buyer_tax_id} onChange={event => setManualForm(form => ({ ...form, buyer_tax_id: event.target.value }))} autoComplete="off" /></label><label className="text-sm">币种<Input className="mt-1 uppercase" value={manualForm.currency} onChange={event => setManualForm(form => ({ ...form, currency: event.target.value }))} maxLength={3} /></label><label className="text-sm">小数位<Select className="mt-1" value={manualForm.minor_unit_scale} onChange={event => setManualForm(form => ({ ...form, minor_unit_scale: event.target.value }))}>{Array.from({ length: 10 }, (_, scale) => <option key={scale} value={scale}>{scale}</option>)}</Select></label><label className="text-sm">含税金额（最小单位）<Input className="mt-1 font-mono" inputMode="numeric" value={manualForm.amount_minor} onChange={event => setManualForm(form => ({ ...form, amount_minor: event.target.value }))} /></label><label className="text-sm">税额（最小单位）<Input className="mt-1 font-mono" inputMode="numeric" value={manualForm.tax_amount_minor} onChange={event => setManualForm(form => ({ ...form, tax_amount_minor: event.target.value }))} /></label><label className="text-sm">开票时间（Asia/Shanghai）<Input type="datetime-local" className="mt-1" value={manualForm.issued_at} onChange={event => setManualForm(form => ({ ...form, issued_at: event.target.value }))} /></label><label className="text-sm">登记原因（必填）<Input className="mt-1" value={manualForm.reason} onChange={event => setManualForm(form => ({ ...form, reason: event.target.value }))} placeholder="人工补录、历史迁移等" /></label>
      </fieldset><DialogFooter><Button variant="outline" onClick={() => { setManualOpen(false); if (!pendingCreate) setManualForm(EMPTY_MANUAL_FORM) }} disabled={manualSubmitting}>取消</Button><Button onClick={() => void submitManual()} disabled={manualSubmitting || Boolean(pendingCreate)}>{manualSubmitting ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <CheckCircle2 className="mr-2 h-4 w-4" />}{pendingCreate ? '等待对账' : '确认登记'}</Button></DialogFooter></DialogContent></Dialog>

      <Dialog open={importOpen} onOpenChange={open => { setImportOpen(open); if (!open && !pendingImport) { updateCsvText(''); setImportReason('') } }}><DialogContent className="max-w-4xl"><DialogHeader><DialogTitle>CSV 导入发票</DialogTitle><DialogDescription>先预览校验，只有当前 UTF-8 内容与完整有效预览逐字节一致时才能确认；预览不会写入台账。</DialogDescription></DialogHeader><div className="space-y-4"><Input type="file" accept=".csv,text/csv" aria-label="选择 UTF-8 CSV 文件" disabled={Boolean(pendingImport) || previewing || importing} onChange={event => { const file = event.target.files?.[0]; if (file) void readCsvFile(file); event.target.value = '' }} /><textarea value={csvText} onChange={event => updateCsvText(event.target.value)} aria-label="CSV 文本内容" disabled={Boolean(pendingImport) || importing} placeholder="也可以在此粘贴 UTF-8 CSV 内容" className="min-h-36 w-full rounded-md border border-input bg-background px-3 py-2 font-mono text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60" /><label className="text-sm">导入原因<Input className="mt-1" value={importReason} disabled={Boolean(pendingImport) || importing} onChange={event => setImportReason(event.target.value)} placeholder="批量补录、历史迁移等" /></label>
        {preview && <div className="rounded-xl border p-4" role="status" aria-live="polite"><div className="mb-3 flex flex-wrap gap-2"><Badge variant={previewCompletelyValid ? 'success' : 'destructive'}>{previewCompletelyValid ? '可导入' : '存在错误或警告'}</Badge><Badge variant="outline">总行数 {preview.row_count}</Badge><Badge variant="outline">有效 {preview.valid_count}</Badge><Badge variant="outline">错误 {preview.error_count}</Badge><Badge variant="outline">问题 {preview.issue_count ?? 0}</Badge></div>{(preview.errors?.length ?? 0) > 0 && <div className="mb-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"><p className="mb-1 font-medium">CSV 级错误</p><ul className="list-disc space-y-1 pl-5">{preview.errors?.map((error, index) => <li key={`${error.code}-${index}`}>{error.field ? `${error.field}: ` : ''}{error.message}</li>)}</ul></div>}<div className="max-h-52 overflow-auto"><Table><TableHeader><TableRow><TableHead>行</TableHead><TableHead>结果</TableHead><TableHead>发票号</TableHead><TableHead>说明</TableHead></TableRow></TableHeader><TableBody>{preview.rows.map(row => <TableRow key={row.row_number}><TableCell>{row.row_number}</TableCell><TableCell>{row.valid ? <Badge variant="success">有效</Badge> : <Badge variant="destructive">错误</Badge>}</TableCell><TableCell className="font-mono text-xs">{row.invoice?.invoice_number || '-'}</TableCell><TableCell className="text-xs text-muted-foreground">{row.errors?.map(error => `${error.field ? `${error.field}: ` : ''}${error.message}`).join('；') || '通过校验'}</TableCell></TableRow>)}</TableBody></Table></div></div>}
      </div><DialogFooter><Button variant="outline" onClick={() => void previewCsv()} disabled={previewing || importing || Boolean(pendingImport)}>{previewing ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Eye className="mr-2 h-4 w-4" />}预览校验</Button><Button onClick={() => void confirmImport()} disabled={importing || Boolean(pendingImport) || !previewCompletelyValid || !previewMatchesCurrentCsv}>{importing ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Upload className="mr-2 h-4 w-4" />}{pendingImport ? '等待对账' : '确认导入'}</Button></DialogFooter></DialogContent></Dialog>

      <Dialog open={voidOpen} onOpenChange={open => { setVoidOpen(open); if (!open) setVoidReason('') }}><DialogContent><DialogHeader><DialogTitle>确认作废发票</DialogTitle><DialogDescription>此操作不会删除历史，将追加作废事件并影响净已开票金额。作废时间由服务端生成，服务端仅允许管理员执行。</DialogDescription></DialogHeader><div className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm"><p className="font-medium">{detail?.document.invoice_number}</p><p className="text-muted-foreground">{detail?.document.buyer_name} · {detail?.document ? formatMinorAmount(detail.document.amount_minor, detail.document.currency, detail.document.minor_unit_scale) : '-'}</p></div><label className="text-sm">作废原因<Input className="mt-1" value={voidReason} disabled={Boolean(pendingVoid) || voiding} onChange={event => setVoidReason(event.target.value)} placeholder="必填，将写入审计事件" /></label><DialogFooter><Button variant="outline" onClick={() => { setVoidOpen(false); setVoidReason('') }} disabled={voiding}>取消</Button><Button variant="destructive" onClick={() => void confirmVoid()} disabled={voiding || Boolean(pendingVoid)}>{voiding ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <Ban className="mr-2 h-4 w-4" />}{pendingVoid ? '等待对账' : '确认作废'}</Button></DialogFooter></DialogContent></Dialog>
    </div>
  )
}
