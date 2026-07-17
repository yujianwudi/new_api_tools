export type MinorAmount = number | string

export interface InvoiceSummaryGroup {
  currency: string
  minor_unit_scale: number
  blue_issued_minor: MinorAmount
  red_issued_minor: MinorAmount
  voided_blue_minor: MinorAmount
  voided_red_minor: MinorAmount
  voided_minor: MinorAmount
  net_issued_minor: MinorAmount
  voided_count: number
  effective_count: number
  anomaly_count?: number
}

export interface InvoiceFilters {
  currency: string
  status: '' | 'issued' | 'voided'
  issuedFrom: string
  issuedTo: string
}

export class InvoiceApiError extends Error {
  status: number
  code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'InvoiceApiError'
    this.status = status
    this.code = code
  }
}

function minorAmountToBigInt(value: MinorAmount): bigint | null {
  if (typeof value === 'number') {
    if (!Number.isSafeInteger(value)) return null
    return BigInt(value)
  }
  const normalized = value.trim()
  if (!/^-?\d+$/.test(normalized)) return null
  try {
    return BigInt(normalized)
  } catch {
    return null
  }
}

export function formatMinorAmount(
  value: MinorAmount | null | undefined,
  currency: string,
  scale: number,
): string {
  if (value === null || value === undefined) return '未提供'
  if (!Number.isInteger(scale) || scale < 0 || scale > 9) return '金额格式异常'
  const amount = minorAmountToBigInt(value)
  if (amount === null) return '金额超出安全范围'

  const negative = amount < 0n
  const absolute = negative ? -amount : amount
  const divisor = 10n ** BigInt(scale)
  const whole = absolute / divisor
  const fraction = scale > 0 ? (absolute % divisor).toString().padStart(scale, '0') : ''
  const groupedWhole = whole.toString().replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const digits = `${negative ? '-' : ''}${groupedWhole}${scale > 0 ? `.${fraction}` : ''}`
  const code = currency.trim().toUpperCase() || 'UNKNOWN'
  return `${code} ${digits}`
}

export function formatInvoiceTime(value?: string | null): string {
  if (!value) return '-'
  const time = new Date(value)
  if (Number.isNaN(time.getTime())) return '时间格式异常'
  return time.toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function shanghaiDateBoundary(date: string, nextDay: boolean): string {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) throw new Error('日期格式无效')
  const [year, month, day] = date.split('-').map(Number)
  const base = new Date(Date.UTC(year, month - 1, day))
  if (base.getUTCFullYear() !== year || base.getUTCMonth() !== month - 1 || base.getUTCDate() !== day) throw new Error('日期格式无效')
  const value = nextDay ? new Date(base.getTime() + 24 * 60 * 60 * 1000) : base
  const output = [value.getUTCFullYear(), String(value.getUTCMonth() + 1).padStart(2, '0'), String(value.getUTCDate()).padStart(2, '0')].join('-')
  return `${output}T00:00:00+08:00`
}

export function validateInvoiceFilters(filters: InvoiceFilters): string | null {
  if (filters.currency.trim() && !/^[A-Za-z]{3}$/.test(filters.currency.trim())) {
    return '币种必须是 3 位字母代码，例如 CNY'
  }
  if (filters.issuedFrom && filters.issuedTo && filters.issuedFrom > filters.issuedTo) {
    return '开始日期不能晚于结束日期'
  }
  try {
    if (filters.issuedFrom) shanghaiDateBoundary(filters.issuedFrom, false)
    if (filters.issuedTo) shanghaiDateBoundary(filters.issuedTo, true)
  } catch (error) {
    return error instanceof Error ? error.message : '日期格式无效'
  }
  return null
}

export function invoiceFilterParams(filters: InvoiceFilters, includeStatus = true): URLSearchParams {
  const params = new URLSearchParams()
  if (filters.currency.trim()) params.set('currency', filters.currency.trim().toUpperCase())
  if (includeStatus && filters.status) params.set('status', filters.status)
  if (filters.issuedFrom) params.set('issued_from', shanghaiDateBoundary(filters.issuedFrom, false))
  if (filters.issuedTo) params.set('issued_to', shanghaiDateBoundary(filters.issuedTo, true))
  return params
}

export function effectiveInvoiceCount(group: InvoiceSummaryGroup): number | null {
  if (Number.isSafeInteger(group.effective_count) && (group.effective_count ?? -1) >= 0) {
    return group.effective_count ?? null
  }
  return null
}

export function invoiceErrorMessage(status: number, fallback?: string): string {
  if (status === 401) return '登录已过期，请重新登录'
  if (status === 403) return '当前账号没有执行此操作的权限'
  if (status === 409) return fallback || '数据发生冲突，可能存在重复发票号或并发操作'
  if (status === 412) return fallback || '操作缺少幂等凭据，请刷新页面后重试'
  if (status === 422) return fallback || '提交内容未通过校验，请检查标记字段'
  if (status === 400) return fallback || '请求参数无效，请检查日期和表单内容'
  if (status === 503) return '发票台账暂不可用，当前数据不会按 0 展示'
  return fallback || `请求失败（HTTP ${status}）`
}

export function sourceFreshness(generatedAt?: string | null, now = Date.now()): 'fresh' | 'stale' | 'unknown' {
  if (!generatedAt) return 'unknown'
  const timestamp = new Date(generatedAt).getTime()
  if (!Number.isFinite(timestamp)) return 'unknown'
  return now - timestamp > 15 * 60 * 1000 ? 'stale' : 'fresh'
}

export function decodeUtf8Csv(buffer: ArrayBuffer): string {
  const decoded = new TextDecoder('utf-8', { fatal: true }).decode(buffer)
  return decoded.replace(/^\uFEFF/, '')
}

export async function sha256Utf8(value: string): Promise<string> {
  if (!globalThis.crypto?.subtle) throw new Error('当前浏览器不支持安全内容摘要')
  const digest = await globalThis.crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, '0')).join('')
}
