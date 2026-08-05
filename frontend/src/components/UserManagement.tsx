import { useState, useEffect, useCallback, useRef } from 'react'
import { useAuth } from '../contexts/AuthContext'
import { useToast } from './Toast'
import {
  Users,
  UserCheck,
  UserX,
  Clock,
  Search,
  Trash2,
  Loader2,
  ChevronLeft,
  ChevronRight,
  AlertTriangle,
  RefreshCw,
  Eye,
  ShieldCheck,
  Github,
  MessageCircle,
  Send,
  Key,
  Shield,
} from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle } from './ui/card'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Badge } from './ui/badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from './ui/table'
import { Select } from './ui/select'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from './ui/dialog'
import { StatCard } from './StatCard'
import { cn } from '../lib/utils'
import { UserAnalysisDialog } from './UserAnalysisDialog'
import { Tabs, TabsContent, TabsList, TabsTrigger } from './ui/tabs'
import { AffiliateStats } from './AffiliateStats'
import {
  beginPendingMutation,
  bindOperationReleaseCandidate,
  clearPendingMutation,
  fetchOperationReconciliation,
  getPendingMutation,
  idempotencyHeader,
  indexPendingMutationsByOperation,
  listPendingMutations,
  operationReconciliationAction,
  operationReconciliationDecision,
  operationReleaseCandidateMatches,
  parseUserMutationOperationTargetId,
  userMutationOperationIdentifier,
  type OperationReleaseCandidate,
  type PendingMutationRecord,
  type PendingMutationSnapshot,
} from '../lib/idempotency'
import {
  canSafelyHardDelete,
  fetchNewAPICapabilities,
  type NewAPICapabilityData,
} from '../lib/controlPlane'



interface ActivityStats {
  total_users: number
  active_users: number | null
  inactive_users: number | null
  very_inactive_users: number | null
  never_requested: number
  source_state?: 'fresh' | 'partial' | 'unavailable'
  as_of?: number
}

type StatsDataState = 'loading' | 'partial' | 'fresh' | 'unavailable'

// 分组信息
interface GroupInfo {
  group_name: string
  user_count: number
}

// 注册来源标签
const SOURCE_LABELS: Record<string, { label: string; icon: typeof Github }> = {
  github: { label: 'GitHub', icon: Github },
  wechat: { label: '微信', icon: MessageCircle },
  telegram: { label: 'Telegram', icon: Send },
  discord: { label: 'Discord', icon: MessageCircle },
  oidc: { label: 'OIDC', icon: Shield },
  linux_do: { label: 'LinuxDO', icon: Users },
  password: { label: '密码注册', icon: Key },
}

const SOFT_DELETE_CONFIRM_TEXT = '注销用户'
const HARD_DELETE_CONFIRM_TEXT = '彻底删除'

interface UserInfo {
  id: number
  username: string
  display_name: string | null
  email: string | null
  role: number
  status: number
  quota: number
  used_quota: number
  request_count: number
  group: string | null
  last_request_time: number | null
  activity_level: string
  linux_do_id: string | null
  source?: string
}

type QueryDataState = 'loading' | 'fresh' | 'stale' | 'unavailable' | 'empty'
const INVITED_USERS_PAGE_SIZE = 10
const SHA256_HEX_PATTERN = /^[0-9a-f]{64}$/

class InvitedSnapshotContractError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'InvitedSnapshotContractError'
  }
}

interface InvitedUserInfo {
  user_id: number
  username: string
  display_name?: string | null
  email?: string
  status: number
  quota?: number
  used_quota?: number
  request_count: number
  group: string
  role?: number
}

interface InvitedUsersData {
  inviter: {
    user_id: number
    username: string
    display_name?: string | null
    aff_code?: string
    aff_count: number
    aff_quota?: number
    aff_history?: number
  }
  items: InvitedUserInfo[]
  total: number
  page: number
  page_size: number
  total_pages: number
  as_of: number
  query_fingerprint: string
  stats: {
    total_invited: number
    requested_count: number
    banned_count: number
    total_used_quota?: number
    total_requests: number
  }
}

interface InvitedUsersQueryIdentity {
  userId: number
  pageSize: number
  asOf: number
  queryFingerprint: string
}

interface DeleteUserMutationPayload extends Record<string, unknown> {
  userId: number
  username: string
  activityLevel: string
  hardDelete: boolean
  reason: string
  confirmText: string
}

type DeleteUserPendingMutation = PendingMutationRecord | PendingMutationSnapshot<DeleteUserMutationPayload>

export function UserManagement() {
  const { token } = useAuth()
  const { showToast } = useToast()

  const [activeTab, setActiveTab] = useState<'list' | 'affiliate'>('list')
  const [stats, setStats] = useState<ActivityStats | null>(null)
  const [statsState, setStatsState] = useState<StatsDataState>('loading')
  const statsRequestRef = useRef<{ sequence: number; controller: AbortController | null }>({
    sequence: 0,
    controller: null,
  })
  const statsFullFetchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const [users, setUsers] = useState<UserInfo[]>([])
  const [loading, setLoading] = useState(true)
  const [userQueryState, setUserQueryState] = useState<QueryDataState>('loading')
  const [userQueryError, setUserQueryError] = useState<string | null>(null)
  const [displayedQuery, setDisplayedQuery] = useState('全部用户')
  const usersRef = useRef<UserInfo[]>([])
  const usersRequestRef = useRef<{ sequence: number; key: string; controller: AbortController | null }>({
    sequence: 0,
    key: '',
    controller: null,
  })
  const [page, setPage] = useState(1)
  const [pageSize] = useState(20)
  const [total, setTotal] = useState(0)
  const [totalPages, setTotalPages] = useState(0)
  const [search, setSearch] = useState('')
  const [searchInput, setSearchInput] = useState('')
  const [activityFilter, setActivityFilter] = useState<string>('all')
  const [deleting, setDeleting] = useState(false)
  const [batchPreviewing, setBatchPreviewing] = useState<string | null>(null)
  const [refreshing, setRefreshing] = useState(false)
  const [newAPICapabilities, setNewAPICapabilities] = useState<NewAPICapabilityData | null>(null)
  const [capabilitiesLoading, setCapabilitiesLoading] = useState(true)
  const [capabilitiesError, setCapabilitiesError] = useState<string | null>(null)

  // 软删除用户清理
  const [softDeletedCount, setSoftDeletedCount] = useState(0)
  const [purgePreviewing, setPurgePreviewing] = useState(false)

  const [confirmDialog, setConfirmDialog] = useState<{
    isOpen: boolean
    title: string
    message: string
    type: 'warning' | 'danger'
    onConfirm: () => void
    details?: { count: number; users: string[] }
    loading?: boolean
    activityLevel?: string
    hardDelete?: boolean
    requireConfirmText?: boolean
    confirmText?: string
    previewOnly?: boolean
  }>({
    isOpen: false,
    title: '',
    message: '',
    type: 'warning',
    onConfirm: () => { },
  })

  // 删除类高风险操作的二次确认输入
  const [deleteConfirmText, setDeleteConfirmText] = useState('')
  const [deleteReason, setDeleteReason] = useState('')
  const [deleteUserTarget, setDeleteUserTarget] = useState<{ userId: number; username: string; activityLevel: string } | null>(null)
  const [pendingDeleteMutations, setPendingDeleteMutations] = useState<Map<string, DeleteUserPendingMutation>>(
    () => indexPendingMutationsByOperation<DeleteUserPendingMutation>(listPendingMutations(), 'user'),
  )
  const [deleteReleaseCandidate, setDeleteReleaseCandidate] = useState<OperationReleaseCandidate | null>(null)
  const [deleteReconciling, setDeleteReconciling] = useState(false)
  const pendingDeleteMutation = deleteUserTarget
    ? pendingDeleteMutations.get(userMutationOperationIdentifier(deleteUserTarget.userId)) ?? null
    : null
  const pendingDeleteNotice = pendingDeleteMutations.values().next().value ?? null
  const deletePayloadLocked = pendingDeleteMutation !== null
  const deleteReconciliationRequired = pendingDeleteMutation?.reconciliationRequired === true

  const rememberPendingDeleteMutation = (pending: DeleteUserPendingMutation) => {
    setPendingDeleteMutations(current => {
      const next = new Map(current)
      next.set(pending.operationIdentifier, pending)
      return next
    })
  }

  const forgetPendingDeleteMutation = (pending: DeleteUserPendingMutation) => {
    setPendingDeleteMutations(current => {
      if (!current.has(pending.operationIdentifier)) return current
      const next = new Map(current)
      next.delete(pending.operationIdentifier)
      return next
    })
  }

  // 用户分析弹窗状态
  const [analysisDialogOpen, setAnalysisDialogOpen] = useState(false)
  const [selectedUser, setSelectedUser] = useState<{ id: number; username: string } | null>(null)

  // 邀请用户列表状态
  const [invitedUsers, setInvitedUsers] = useState<InvitedUsersData | null>(null)
  const [invitedLoading, setInvitedLoading] = useState(false)
  const [invitedState, setInvitedState] = useState<QueryDataState>('loading')
  const [invitedError, setInvitedError] = useState<string | null>(null)
  const [invitedPage, setInvitedPage] = useState(1)
  const invitedRequestRef = useRef<{ sequence: number; key: string; controller: AbortController | null }>({
    sequence: 0,
    key: '',
    controller: null,
  })
  const invitedUsersRef = useRef<InvitedUsersData | null>(null)
  const invitedIdentityRef = useRef<InvitedUsersQueryIdentity | null>(null)

  // 批量分组管理状态
  const [groups, setGroups] = useState<GroupInfo[]>([])
  const [groupFilter, setGroupFilter] = useState('')
  const [sourceFilter, setSourceFilter] = useState('')
  const [selectedUserIds, setSelectedUserIds] = useState<Set<number>>(new Set())

  // Linux.do 用户名查询状态
  const [linuxDoLookupLoading, setLinuxDoLookupLoading] = useState<string | null>(null)

  const allSelectedOnPage = users.length > 0 && users.every((u) => selectedUserIds.has(u.id))
  const listInteractionDisabled = loading || userQueryState === 'stale' || userQueryState === 'unavailable'

  const toggleSelectAllOnPage = () => {
    setSelectedUserIds((prev) => {
      const next = new Set(prev)
      const allSelected = users.length > 0 && users.every((u) => next.has(u.id))
      if (allSelected) {
        users.forEach((u) => next.delete(u.id))
      } else {
        users.forEach((u) => next.add(u.id))
      }
      return next
    })
  }

  const toggleSelectUser = (userId: number) => {
    setSelectedUserIds((prev) => {
      const next = new Set(prev)
      if (next.has(userId)) next.delete(userId)
      else next.add(userId)
      return next
    })
  }

  const apiUrl = import.meta.env.VITE_API_URL || ''

  const getAuthHeaders = useCallback(() => ({
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }), [token])

  const loadNewAPICapabilities = useCallback(async (signal?: AbortSignal) => {
    setCapabilitiesLoading(true)
    setCapabilitiesError(null)
    try {
      const data = await fetchNewAPICapabilities({ apiUrl, token, signal })
      if (!signal?.aborted) setNewAPICapabilities(data)
      return !signal?.aborted
    } catch (error) {
      if (!signal?.aborted) {
        setNewAPICapabilities(null)
        setCapabilitiesError(error instanceof Error ? error.message : '能力探测失败')
      }
      return false
    } finally {
      if (!signal?.aborted) setCapabilitiesLoading(false)
    }
  }, [apiUrl, token])

  const hardDeleteAvailable = canSafelyHardDelete(newAPICapabilities)

  const activityStatValue = (value: number | null | undefined, availableInPartial = false): number | string => {
    if (typeof value !== 'number') return 'N/A'
    if (statsState === 'fresh' || (statsState === 'partial' && availableInPartial)) return value
    return 'N/A'
  }
  const activityStatHint = (freshLabel: string, availableInPartial = false): string => {
    if (statsState === 'fresh') return freshLabel
    if (statsState === 'partial') return availableInPartial ? '快速统计；完整活跃度加载中' : '等待完整日志证据'
    if (statsState === 'unavailable') return '统计来源不可用'
    return '正在加载'
  }

  const fetchStats = useCallback(async (quick = false) => {
    statsRequestRef.current.controller?.abort()
    if (statsFullFetchTimerRef.current !== null) {
      clearTimeout(statsFullFetchTimerRef.current)
      statsFullFetchTimerRef.current = null
    }
    const controller = new AbortController()
    const sequence = statsRequestRef.current.sequence + 1
    statsRequestRef.current = { sequence, controller }
    try {
      const params = quick ? '?quick=true' : ''
      const response = await fetch(`${apiUrl}/api/users/stats${params}`, {
        headers: getAuthHeaders(),
        signal: controller.signal,
      })
      const data = await response.json()
      if (statsRequestRef.current.sequence !== sequence || controller.signal.aborted) return false
      if (!response.ok || !data.success || !data.data) {
        throw new Error(data.error?.message || '用户活跃度统计不可用')
      }
      setStats(data.data)
      if (quick) {
        setStatsState('partial')
        // 快速响应只包含总数与“从未请求”；无论这些值是否为 0，
        // 都必须继续获取完整证据，不能把未计算字段当成可信零。
        statsFullFetchTimerRef.current = setTimeout(() => {
          statsFullFetchTimerRef.current = null
          if (statsRequestRef.current.sequence === sequence && !controller.signal.aborted) {
            void fetchStats(false)
          }
        }, 100)
      } else {
        setStatsState(data.data.source_state === 'fresh'
          ? 'fresh'
          : data.data.source_state === 'partial' ? 'partial' : 'unavailable')
      }
      return true
    } catch (error) {
      if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError')) return false
      if (statsRequestRef.current.sequence !== sequence) return false
      console.error('Failed to fetch stats:', error)
      setStatsState('unavailable')
      return false
    }
  }, [apiUrl, getAuthHeaders])

  // 获取软删除用户数量
  const fetchSoftDeletedCount = useCallback(async () => {
    try {
      const response = await fetch(`${apiUrl}/api/users/soft-deleted/count`, { headers: getAuthHeaders() })
      const data = await response.json()
      if (data.success) {
        setSoftDeletedCount(data.data?.count || 0)
      }
    } catch (error) {
      console.error('Failed to fetch soft deleted count:', error)
    }
  }, [apiUrl, getAuthHeaders])

  // 获取可用分组列表
  const fetchGroups = useCallback(async () => {
    try {
      const response = await fetch(`${apiUrl}/api/auto-group/groups`, { headers: getAuthHeaders() })
      const data = await response.json()
      if (response.ok && data.success) {
        const normalized = new Map<string, number>()
        for (const item of (data.data?.items || []) as GroupInfo[]) {
          const name = item.group_name?.trim() || 'default'
          normalized.set(name, (normalized.get(name) || 0) + Number(item.user_count || 0))
        }
        setGroups(Array.from(normalized, ([group_name, user_count]) => ({ group_name, user_count })))
      }
    } catch (error) {
      console.error('Failed to fetch groups:', error)
    }
  }, [apiUrl, getAuthHeaders])

  // 批量移动用户到指定分组

  // 预览清理软删除用户
  const previewPurgeSoftDeleted = async () => {
    setDeleteConfirmText('')
    setPurgePreviewing(true)
    setConfirmDialog({
      isOpen: true,
      title: '已软删除用户预览（只读）',
      message: '正在查询已软删除的用户...',
      type: 'warning',
      loading: true,
      hardDelete: true,
      previewOnly: true,
      onConfirm: () => setConfirmDialog(prev => ({ ...prev, isOpen: false })),
    })

    try {
      const response = await fetch(`${apiUrl}/api/users/soft-deleted/purge`, {
        method: 'POST',
        headers: getAuthHeaders(),
        body: JSON.stringify({ dry_run: true }),
      })
      const data = await response.json()
      if (data.success && data.data) {
        const count = data.data.count ?? data.data.affected_count ?? data.data.affected ?? 0
        const usernames = Array.isArray(data.data.users) ? data.data.users : []
        if (count === 0) {
          setConfirmDialog(prev => ({ ...prev, isOpen: false }))
          showToast('info', '没有需要清理的软删除用户')
          return
        }
        setConfirmDialog(prev => ({
          ...prev,
          message: `发现 ${count} 个已软删除用户。\n\nv0.5 禁止旁路批量写，此页面只展示候选对象，不会执行永久清理。请在 NewAPI 管理端逐个复核。`,
          details: { count, users: usernames },
          loading: false,
        }))
      } else {
        setConfirmDialog(prev => ({ ...prev, isOpen: false }))
        showToast('error', data.message || '预览失败')
      }
    } catch (error) {
      console.error('Failed to preview purge:', error)
      setConfirmDialog(prev => ({ ...prev, isOpen: false }))
      showToast('error', '预览失败')
    } finally {
      setPurgePreviewing(false)
    }
  }

  const fetchUsers = useCallback(async () => {
    usersRequestRef.current.controller?.abort()
    const controller = new AbortController()
    const sequence = usersRequestRef.current.sequence + 1
    setLoading(true)
    setUserQueryState('loading')
    setUserQueryError(null)
    const queryDescription = [
      search ? `搜索“${search}”` : '',
      activityFilter !== 'all' ? `活跃度:${activityFilter}` : '',
      groupFilter ? `分组:${groupFilter}` : '',
      sourceFilter ? `来源:${SOURCE_LABELS[sourceFilter]?.label || sourceFilter}` : '',
    ].filter(Boolean).join(' · ') || '全部用户'
    try {
      const params = new URLSearchParams({
        page: page.toString(),
        page_size: pageSize.toString(),
      })
      if (search) params.append('search', search)
      if (activityFilter && activityFilter !== 'all') params.append('activity', activityFilter)
      if (groupFilter) params.append('group', groupFilter)
      if (sourceFilter) params.append('source', sourceFilter)

      const requestKey = params.toString()
      usersRequestRef.current = { sequence, key: requestKey, controller }
      const response = await fetch(`${apiUrl}/api/users?${params}`, {
        headers: getAuthHeaders(),
        signal: controller.signal,
      })
      const data = await response.json()
      const isLatest = usersRequestRef.current.sequence === sequence
        && usersRequestRef.current.key === requestKey
        && !controller.signal.aborted
      if (!isLatest) return false
      if (!response.ok || !data.success) {
        throw new Error(data?.error?.message || `用户查询失败（HTTP ${response.status}）`)
      }
      if (Number(data.data?.page) !== page || Number(data.data?.page_size) !== pageSize) {
        throw new Error('用户列表响应与当前分页条件不一致')
      }
      const nextUsers = Array.isArray(data.data?.items) ? data.data.items : []
      usersRef.current = nextUsers
      setUsers(nextUsers)
      setTotal(data.data.total)
      setTotalPages(data.data.total_pages)
      setDisplayedQuery(queryDescription)
      setUserQueryState(nextUsers.length > 0 ? 'fresh' : 'empty')
      return true
    } catch (error) {
      if (controller.signal.aborted || (error instanceof DOMException && error.name === 'AbortError')) return false
      if (usersRequestRef.current.sequence !== sequence) return false
      console.error('Failed to fetch users:', error)
      const message = error instanceof Error ? error.message : '加载用户列表失败'
      setUserQueryError(message)
      setUserQueryState(usersRef.current.length > 0 ? 'stale' : 'unavailable')
      showToast('error', message)
      return false
    } finally {
      if (usersRequestRef.current.sequence === sequence && !controller.signal.aborted) {
        setLoading(false)
      }
    }
  }, [apiUrl, getAuthHeaders, page, pageSize, search, activityFilter, groupFilter, sourceFilter, showToast])

  // 单个用户删除状态
  const [deleteMode, setDeleteMode] = useState<'soft' | 'hard'>('soft')

  const openPendingMutationDialog = (pending: DeleteUserPendingMutation, username?: string) => {
    let userId: number
    try {
      userId = parseUserMutationOperationTargetId(pending.operationIdentifier, pending.targetId)
    } catch (error) {
      showToast(
        'error',
        error instanceof Error
          ? `待对账操作的用户 ID 无效，持久锁已保留：${error.message}`
          : '待对账操作的用户 ID 无效，持久锁已保留',
      )
      return
    }
    rememberPendingDeleteMutation(pending)
    setDeleteReleaseCandidate(current => operationReleaseCandidateMatches(current, pending) ? current : null)
    setDeleteUserTarget({
      userId,
      username: username || `用户 #${pending.targetId}`,
      activityLevel: '',
    })
    setDeleteMode(pending.action === 'user.hard_delete' ? 'hard' : 'soft')
    setDeleteConfirmText('')
    setDeleteReason('')
    setConfirmDialog({
      isOpen: true,
      title: '用户操作待对账',
      message: `操作 ${pending.action}（用户 #${pending.targetId}）已有持久化幂等所有权，只能依据 Tool Store 审计结果解除锁定。`,
      type: 'danger',
      onConfirm: () => { },
    })
  }

  const deleteUser = async (userId: number, username: string) => {
    const existing = getPendingMutation(userMutationOperationIdentifier(userId))
    if (existing) {
      openPendingMutationDialog(existing, username)
      showToast('error', '该用户已有待对账操作，请先读取 Tool Store 审计结果')
      return
    }
    const userToDelete = users.find(u => u.id === userId)
    setDeleteReleaseCandidate(null)
    setDeleteUserTarget({ userId, username, activityLevel: userToDelete?.activity_level || '' })
    setDeleteMode('soft')
    setDeleteConfirmText('')
    setDeleteReason('')
    setConfirmDialog({
      isOpen: true,
      title: '删除用户',
      message: `请选择删除方式：`,
      type: 'danger',
      onConfirm: () => { }, // 占位，实际执行在按钮的 onClick 中处理
    })
  }

  const executeDeleteUser = async () => {
    if (!deleteUserTarget) return
    if (pendingDeleteMutation) {
      showToast('error', '当前用户操作已有持久化幂等所有权，请先对账')
      return
    }

    const { userId, username, activityLevel } = deleteUserTarget
    const hardDelete = deleteMode === 'hard'
    const reason = deleteReason.trim()
    const confirmText = deleteConfirmText.trim()
    if (hardDelete && !hardDeleteAvailable) {
      setDeleteMode('soft')
      setDeleteConfirmText('')
      showToast('error', 'NewAPI 能力探测未确认安全硬删除，已切回注销模式')
      return
    }
    const action = hardDelete ? 'user.hard_delete' : 'user.delete'
    const operationIdentifier = userMutationOperationIdentifier(userId)
    let pending: PendingMutationSnapshot<DeleteUserMutationPayload>
    try {
      pending = beginPendingMutation({
        operationIdentifier,
        fingerprint: JSON.stringify({ action, target_id: String(userId), reason, confirm_text: confirmText }),
        action,
        targetType: 'user',
        targetId: String(userId),
        payload: { userId, username, activityLevel, hardDelete, reason, confirmText },
      })
    } catch (error) {
      const existing = getPendingMutation(operationIdentifier)
      if (existing) openPendingMutationDialog(existing, username)
      showToast('error', error instanceof Error ? error.message : '该用户已有待对账操作')
      return
    }

    rememberPendingDeleteMutation(pending)
    setDeleteReleaseCandidate(null)
    setDeleting(true)
    try {
      const response = await fetch(`${apiUrl}/api/users/${pending.payload.userId}?hard_delete=${pending.payload.hardDelete}`, {
        method: 'DELETE',
        headers: { ...getAuthHeaders(), ...idempotencyHeader(pending.key) },
        body: JSON.stringify({
          confirm_text: pending.payload.confirmText,
          reason: pending.payload.reason,
        }),
      })
      const data = await response.json()
      if (data.success) {
        const lockCleared = clearPendingMutation(pending)
        if (lockCleared) {
          forgetPendingDeleteMutation(pending)
        } else {
          rememberPendingDeleteMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
        }
        if (lockCleared) setDeleteReleaseCandidate(null)
        showToast(
          lockCleared ? 'success' : 'error',
          lockCleared
            ? data.message
            : '操作已生效并写入审计，但浏览器未能清理本地锁；请保留页面并重新对账',
        )
        // 直接从本地状态移除用户，避免重新加载
        setUsers(prev => prev.filter(u => u.id !== pending.payload.userId))
        setTotal(prev => prev - 1)
        // 更新统计数据（本地计算）
        if (stats) {
          setStats(prev => prev ? {
            ...prev,
            total_users: prev.total_users - 1,
            active_users: pending.payload.activityLevel === 'active' && prev.active_users !== null ? Math.max(0, prev.active_users - 1) : prev.active_users,
            inactive_users: pending.payload.activityLevel === 'inactive' && prev.inactive_users !== null ? Math.max(0, prev.inactive_users - 1) : prev.inactive_users,
            very_inactive_users: pending.payload.activityLevel === 'very_inactive' && prev.very_inactive_users !== null ? Math.max(0, prev.very_inactive_users - 1) : prev.very_inactive_users,
            never_requested: pending.payload.activityLevel === 'never' ? prev.never_requested - 1 : prev.never_requested,
          } : null)
        }
        // 如果是软删除，更新软删除计数
        if (!pending.payload.hardDelete) {
          fetchSoftDeletedCount()
        }
        setConfirmDialog(prev => ({ ...prev, isOpen: false }))
        setDeleteConfirmText('')
        setDeleteReason('')
        setDeleteUserTarget(null)
      } else {
        showToast('error', data.error?.message || data.message || '提交未成功；幂等所有权已锁定，请读取审计结果')
      }
    } catch (error) {
      console.error('Failed to delete user:', error)
      showToast('error', '网络中断，幂等所有权已保留；请读取 Tool Store 审计结果，切勿直接重试')
    } finally {
      setDeleting(false)
    }
  }

  const closeDeleteDialog = () => {
    setConfirmDialog(prev => ({ ...prev, isOpen: false }))
    setDeleteConfirmText('')
    setDeleteReason('')
    setDeleteUserTarget(null)
  }

  const reconcileDeleteUser = async () => {
    const pending = pendingDeleteMutation
    if (!pending) return
    setDeleteReconciling(true)
    try {
      const candidateMatches = operationReleaseCandidateMatches(deleteReleaseCandidate, pending)
      const reconciliation = candidateMatches
        ? deleteReleaseCandidate
        : await fetchOperationReconciliation(apiUrl, getAuthHeaders(), pending)
      const decision = operationReconciliationDecision(reconciliation.status)
      const nextAction = operationReconciliationAction(reconciliation.status, candidateMatches)
      if (nextAction === 'keep_locked') {
        setDeleteReleaseCandidate(null)
        const message = reconciliation.status === 'not_found'
          ? 'Tool Store 尚未发现可证明终态的审计记录；原请求可能仍在到达，当前操作继续锁定'
          : reconciliation.status === 'pending'
            ? `审计 #${reconciliation.audit_id} 仍只有操作意图，当前操作继续锁定`
            : `审计 #${reconciliation.audit_id} 已标记 cancelled，结果仍不确定；请在控制平面人工处置`
        showToast('error', message)
        return
      }
      if (nextAction === 'confirm_release') {
        setDeleteReleaseCandidate(bindOperationReleaseCandidate(pending, reconciliation))
        showToast('info', `审计 #${reconciliation.audit_id} 确认为 ${reconciliation.status}；操作未生效。请再次点击“确认解除本地锁”`)
        return
      }
      if (!clearPendingMutation(pending)) {
        rememberPendingDeleteMutation(getPendingMutation(pending.operationIdentifier) ?? pending)
        showToast('error', '审计已确认终态，但浏览器未能安全清理本地锁；请勿重试并再次对账')
        return
      }
      setDeleteReleaseCandidate(null)
      forgetPendingDeleteMutation(pending)
      setConfirmDialog(prev => ({ ...prev, isOpen: false }))
      setDeleteConfirmText('')
      setDeleteReason('')
      setDeleteUserTarget(null)
      showToast(decision === 'applied' ? 'success' : 'info', decision === 'applied'
        ? `审计 #${reconciliation.audit_id} 确认操作已经生效，已解除提交锁`
        : `审计 #${reconciliation.audit_id} 确认为 ${reconciliation.status}，已释放本地所有权，可创建新操作`)
      void Promise.all([fetchUsers(), fetchStats(), fetchSoftDeletedCount()])
    } catch (error) {
      console.error('Failed to reconcile user deletion:', error)
      showToast('error', '对账失败，删除表单保持锁定，请检查网络后重试')
    } finally {
      setDeleteReconciling(false)
    }
  }

  const previewBatchDelete = async (level: string, hardDelete: boolean = false) => {
    setDeleteConfirmText('')
    const levelLabel = level === 'never' ? '从未请求' : level === 'inactive' ? '不活跃' : '非常不活跃'
    const actionLabel = hardDelete ? '彻底删除' : '注销'
    const previewKey = `${level}:${hardDelete ? 'hard' : 'soft'}`
    setBatchPreviewing(previewKey)

    setConfirmDialog({
      isOpen: true,
      title: `批量${actionLabel}候选预览（只读）`,
      message: `正在查询${levelLabel}的用户...`,
      type: 'warning',
      loading: true,
      activityLevel: level,
      hardDelete,
      previewOnly: true,
      onConfirm: () => setConfirmDialog(prev => ({ ...prev, isOpen: false })),
    })

    try {
      const response = await fetch(`${apiUrl}/api/users/batch-delete`, {
        method: 'POST',
        headers: getAuthHeaders(),
        body: JSON.stringify({ activity_level: level, dry_run: true, hard_delete: hardDelete }),
      })
      const data = await response.json()
      if (data.success && data.data) {
        const count = data.data.count ?? data.data.affected_count ?? data.data.affected ?? 0
        const usernames = Array.isArray(data.data.users) ? data.data.users : []
        if (count === 0) {
          setConfirmDialog(prev => ({ ...prev, isOpen: false }))
          showToast('info', '没有符合条件的用户')
          return
        }
        setConfirmDialog(prev => ({
          ...prev,
          message: `发现 ${count} 个${levelLabel}用户。\n\nv0.5 禁止旁路批量写，此页面只展示${actionLabel}候选，不会提交任何用户变更。请在 NewAPI 管理端逐个复核。`,
          details: { count, users: usernames },
          loading: false,
        }))
      } else {
        setConfirmDialog(prev => ({ ...prev, isOpen: false }))
        showToast('error', data.message || '预览失败')
      }
    } catch (error) {
      console.error('Failed to preview batch delete:', error)
      setConfirmDialog(prev => ({ ...prev, isOpen: false }))
      showToast('error', '预览失败')
    } finally {
      setBatchPreviewing(null)
    }
  }

  const handleSearch = () => {
    setPage(1)
    setSearch(searchInput)
  }

  const handleKeyPress = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') handleSearch()
  }

  useEffect(() => {
    void fetchStats(true)  // 首次加载使用快速模式
    void fetchSoftDeletedCount()  // 获取软删除用户数量
    void fetchGroups()  // 获取分组列表
    return () => {
      statsRequestRef.current.controller?.abort()
      if (statsFullFetchTimerRef.current !== null) {
        clearTimeout(statsFullFetchTimerRef.current)
        statsFullFetchTimerRef.current = null
      }
    }
  }, [fetchStats, fetchSoftDeletedCount, fetchGroups])

  useEffect(() => {
    const controller = new AbortController()
    void loadNewAPICapabilities(controller.signal)
    return () => controller.abort()
  }, [loadNewAPICapabilities])

  useEffect(() => {
    void fetchUsers()
    return () => usersRequestRef.current.controller?.abort()
  }, [fetchUsers])

  useEffect(() => {
    usersRef.current = users
  }, [users])

  const handleRefresh = async () => {
    setRefreshing(true)
    const [usersOK, statsOK, capabilitiesOK] = await Promise.all([fetchUsers(), fetchStats(), loadNewAPICapabilities()])
    setRefreshing(false)
    if (usersOK && statsOK && capabilitiesOK) showToast('success', '数据已刷新')
    else showToast('error', '刷新未完整成功，失败数据已明确标记')
  }

  // 打开用户分析弹窗
  const openUserAnalysis = (userId: number, username: string) => {
    if (listInteractionDisabled) return
    invitedRequestRef.current.controller?.abort()
    invitedRequestRef.current.sequence += 1
    setSelectedUser({ id: userId, username })
    setAnalysisDialogOpen(true)
    invitedUsersRef.current = null
    invitedIdentityRef.current = null
    setInvitedUsers(null)
    setInvitedState('loading')
    setInvitedError(null)
    setInvitedPage(1)
  }

  const handleAnalysisOpenChange = (open: boolean) => {
    if (!open) {
      invitedRequestRef.current.controller?.abort()
      invitedRequestRef.current.sequence += 1
      invitedUsersRef.current = null
      invitedIdentityRef.current = null
      setInvitedUsers(null)
      setInvitedState('loading')
      setInvitedError(null)
    }
    setAnalysisDialogOpen(open)
  }

  // 获取邀请用户列表
  const fetchInvitedUsers = useCallback(async () => {
    if (!selectedUser || !analysisDialogOpen) return
    const expectedIdentity = invitedIdentityRef.current
    if (invitedPage !== 1 && (!expectedIdentity || expectedIdentity.userId !== selectedUser.id || expectedIdentity.pageSize !== INVITED_USERS_PAGE_SIZE)) {
      invitedUsersRef.current = null
      invitedIdentityRef.current = null
      setInvitedUsers(null)
      setInvitedError('邀请分页缺少第一页快照身份，请关闭后重新打开详情')
      setInvitedState('unavailable')
      setInvitedLoading(false)
      return
    }
    invitedRequestRef.current.controller?.abort()
    const controller = new AbortController()
    const sequence = invitedRequestRef.current.sequence + 1
    const params = new URLSearchParams({
      page: String(invitedPage),
      page_size: String(INVITED_USERS_PAGE_SIZE),
    })
    if (expectedIdentity) {
      params.set('as_of', String(expectedIdentity.asOf))
      params.set('query_fingerprint', expectedIdentity.queryFingerprint)
    }
    const requestKey = `${selectedUser.id}:${params.toString()}`
    invitedRequestRef.current = { sequence, key: requestKey, controller }
    setInvitedLoading(true)
    setInvitedState('loading')
    setInvitedError(null)
    try {
      const response = await fetch(`${apiUrl}/api/users/${selectedUser.id}/invited?${params.toString()}`, {
        headers: getAuthHeaders(),
        signal: controller.signal,
      })
      const res = await response.json()
      const isLatest = invitedRequestRef.current.sequence === sequence
        && invitedRequestRef.current.key === requestKey
        && !controller.signal.aborted
      if (!isLatest) return
      if (!response.ok || !res.success) {
        if (response.status === 409) {
          throw new InvitedSnapshotContractError(res?.error?.message || '邀请用户快照已变化，请重新打开详情')
        }
        throw new Error(res?.error?.message || `邀请详情查询失败（HTTP ${response.status}）`)
      }
      const data = res.data as InvitedUsersData
      const total = data?.total
      const totalPages = data?.total_pages
      const expectedTotalPages = Number.isSafeInteger(total) && total >= 0
        ? (total === 0 ? 0 : Math.ceil(total / INVITED_USERS_PAGE_SIZE))
        : -1
      const expectedItemCount = Number.isSafeInteger(total) && total >= 0
        ? Math.max(0, Math.min(INVITED_USERS_PAGE_SIZE, total - (invitedPage - 1) * INVITED_USERS_PAGE_SIZE))
        : -1
      const responseIdentityValid = Number.isSafeInteger(data?.as_of)
        && data.as_of > 0
        && typeof data?.query_fingerprint === 'string'
        && SHA256_HEX_PATTERN.test(data.query_fingerprint)
      const paginationValid = Number.isSafeInteger(data?.page)
        && data.page === invitedPage
        && Number.isSafeInteger(data?.page_size)
        && data.page_size === INVITED_USERS_PAGE_SIZE
        && Number.isSafeInteger(totalPages)
        && totalPages === expectedTotalPages
        && (invitedPage === 1 || invitedPage <= totalPages)
        && Array.isArray(data?.items)
        && data.items.length === expectedItemCount
        && data.items.every(item => Number.isSafeInteger(item?.user_id) && item.user_id > 0)
      const populationValid = Number.isSafeInteger(data?.inviter?.user_id)
        && data.inviter.user_id === selectedUser.id
        && Number.isSafeInteger(data?.stats?.total_invited)
        && data.stats.total_invited === total
      const expectedIdentityMatches = !expectedIdentity || (
        expectedIdentity.userId === selectedUser.id
        && expectedIdentity.pageSize === data.page_size
        && expectedIdentity.asOf === data.as_of
        && expectedIdentity.queryFingerprint === data.query_fingerprint
      )
      if (!responseIdentityValid || !paginationValid || !populationValid || !expectedIdentityMatches) {
        throw new InvitedSnapshotContractError('邀请详情响应与第一页快照身份或分页契约不一致')
      }
      invitedIdentityRef.current = expectedIdentity || {
        userId: selectedUser.id,
        pageSize: data.page_size,
        asOf: data.as_of,
        queryFingerprint: data.query_fingerprint,
      }
      invitedUsersRef.current = data
      setInvitedUsers(data)
      setInvitedState(data.items.length > 0 ? 'fresh' : 'empty')
    } catch (e) {
      if (controller.signal.aborted || (e instanceof DOMException && e.name === 'AbortError')) return
      if (invitedRequestRef.current.sequence !== sequence || invitedRequestRef.current.key !== requestKey) return
      console.error('Failed to fetch invited users:', e)
      const message = e instanceof Error ? e.message : '邀请详情暂时不可用'
      setInvitedError(message)
      if (e instanceof InvitedSnapshotContractError) {
        invitedUsersRef.current = null
        invitedIdentityRef.current = null
        setInvitedUsers(null)
        setInvitedState('unavailable')
      } else if (invitedUsersRef.current?.page === invitedPage) {
        setInvitedState('stale')
      } else {
        invitedUsersRef.current = null
        setInvitedUsers(null)
        setInvitedState('unavailable')
      }
    } finally {
      if (invitedRequestRef.current.sequence === sequence && !controller.signal.aborted) {
        setInvitedLoading(false)
      }
    }
  }, [apiUrl, getAuthHeaders, selectedUser, analysisDialogOpen, invitedPage])

  useEffect(() => {
    if (analysisDialogOpen && selectedUser) {
      void fetchInvitedUsers()
    }
    return () => invitedRequestRef.current.controller?.abort()
  }, [analysisDialogOpen, selectedUser, invitedPage, fetchInvitedUsers])

  const formatQuota = (quota: number) => `$${(quota / 500000).toFixed(2)}`

  // 格式化最后请求时间
  // `null` means no billable-log timestamp was available; never invent a date.
  const formatLastRequest = (user: UserInfo) => {
    if (user.last_request_time) {
      return new Date(user.last_request_time * 1000).toLocaleString('zh-CN', {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
      })
    }
    if (user.request_count > 0) {
      return <span className="text-muted-foreground">无时间证据</span>
    }
    return <span className="text-muted-foreground">从未</span>
  }

  const getActivityBadge = (level: string) => {
    const baseClass = "w-[92px] justify-center"
    switch (level) {
      case 'active':
        return <Badge variant="success" className={baseClass}>活跃</Badge>
      case 'inactive':
        return <Badge variant="warning" className={baseClass}>不活跃</Badge>
      case 'very_inactive':
        return <Badge variant="destructive" className={baseClass}>非常不活跃</Badge>
      case 'never':
        return <Badge variant="secondary" className={baseClass}>从未请求</Badge>
      default:
        return <Badge variant="outline" className={baseClass}>{level}</Badge>
    }
  }

  const getRoleBadge = (role: number) => {
    const baseClass = "w-[92px] justify-center whitespace-nowrap"
    switch (role) {
      case 1:
        return <Badge variant="outline" className={cn(baseClass, "text-muted-foreground font-normal border-muted-foreground/20")}>普通用户</Badge>
      case 10:
        return <Badge className={cn(baseClass, "bg-blue-500 hover:bg-blue-600 border-none")}>管理员</Badge>
      case 100:
        return (
          <Badge className={cn(baseClass, "bg-gradient-to-r from-amber-500 to-orange-600 hover:from-amber-600 hover:to-orange-700 text-white border-none shadow-sm")}>
            <ShieldCheck className="w-3 h-3 mr-1 shrink-0" />
            超级管理员
          </Badge>
        )
      default:
        return <Badge variant="secondary" className={baseClass}>角色{role}</Badge>
    }
  }

  const getStatusBadge = (status: number) => {
    const baseClass = "w-[64px] justify-center"
    switch (status) {
      case 1:
        return <Badge variant="success" className={baseClass}>正常</Badge>
      case 2:
        return <Badge variant="destructive" className={baseClass}>禁用</Badge>
      default:
        return <Badge variant="outline" className={baseClass}>未知</Badge>
    }
  }

  const currentDialogConfirmText = deleteUserTarget
    ? (deleteMode === 'hard' ? HARD_DELETE_CONFIRM_TEXT : SOFT_DELETE_CONFIRM_TEXT)
    : (confirmDialog.requireConfirmText ? confirmDialog.confirmText : '')
  const confirmActionDisabled = Boolean(
    confirmDialog.loading ||
    deleting ||
    deleteReconciling ||
    deleteReconciliationRequired ||
    (currentDialogConfirmText && deleteConfirmText !== currentDialogConfirmText) ||
    (deleteUserTarget && deleteReason.trim().length < 3) ||
    (deleteUserTarget && deleteMode === 'hard' && !hardDeleteAvailable)
  )

  return (
    <div className="min-w-0 max-w-full space-y-6 animate-in fade-in duration-500 motion-reduce:animate-none">
      {/* Header */}
      <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
        <div>
          <h2 className="text-3xl font-bold tracking-tight">用户管理</h2>
          <p className="text-muted-foreground mt-1">查看和管理所有用户及其状态</p>
        </div>
        <Button variant="outline" size="sm" onClick={handleRefresh} disabled={refreshing || loading} className="h-9">
          <RefreshCw aria-hidden="true" className={cn("h-4 w-4 mr-2", refreshing && "animate-spin motion-reduce:animate-none")} />
          刷新
        </Button>
      </div>

      {pendingDeleteNotice && (
        <div className="flex flex-col gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex items-start gap-3">
            <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-amber-600" />
            <div>
              <p className="font-medium text-foreground">用户操作待审计对账</p>
              <p className="text-sm text-muted-foreground">
                {pendingDeleteNotice.action} · 用户 #{pendingDeleteNotice.targetId}。刷新页面不会解除该锁，只有 Tool Store 的明确终态才能解锁。
              </p>
            </div>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => openPendingMutationDialog(pendingDeleteNotice)}
            disabled={deleting || deleteReconciling}
          >
            <RefreshCw className="mr-2 h-4 w-4" />
            查看并对账
          </Button>
        </div>
      )}

      <Tabs value={activeTab} onValueChange={(v) => setActiveTab(v as 'list' | 'affiliate')} className="w-full">
        <TabsList className="grid w-full max-w-md grid-cols-2">
          <TabsTrigger value="list" className="gap-2">
            <Users className="h-4 w-4" />
            用户列表
          </TabsTrigger>
          <TabsTrigger value="affiliate" className="gap-2">
            <ShieldCheck className="h-4 w-4" />
            邀请充值分析
          </TabsTrigger>
        </TabsList>

        {/* forceMount + data-state hide：保留列表 tab 的状态/筛选/分页，
            切到邀请充值分析再切回不会触发重新拉数据。 */}
        <TabsContent value="list" forceMount className="data-[state=inactive]:hidden mt-6 space-y-6">

      {/* Activity Stats Cards */}
      {statsState === 'partial' && (
        <div role="status" aria-live="polite" className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm text-amber-900 dark:text-amber-100">
          活跃度证据不完整；当前仅“从未请求”可作为可信数字，其余指标不会显示为 0。
        </div>
      )}
      {statsState === 'unavailable' && (
        <div role="alert" className="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm text-destructive">
          活跃度统计来源不可用；未计算或旧值不会显示为可信数字。
        </div>
      )}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
        <StatCard
          title="活跃用户"
          value={activityStatValue(stats?.active_users)}
          subValue={activityStatHint("7天内有请求")}
          icon={UserCheck}
          color="green"
          onClick={() => { setActivityFilter('active'); setPage(1) }}
          className={cn(activityFilter === 'active' && "ring-2 ring-primary ring-offset-2")}
        />
        <StatCard
          title="不活跃用户"
          value={activityStatValue(stats?.inactive_users)}
          subValue={activityStatHint("7-30天内有请求")}
          icon={Clock}
          color="yellow"
          onClick={() => { setActivityFilter('inactive'); setPage(1) }}
          className={cn(activityFilter === 'inactive' && "ring-2 ring-primary ring-offset-2")}
        />
        <StatCard
          title="非常不活跃"
          value={activityStatValue(stats?.very_inactive_users)}
          subValue={activityStatHint("超过30天无请求")}
          icon={UserX}
          color="red"
          onClick={() => { setActivityFilter('very_inactive'); setPage(1) }}
          className={cn(activityFilter === 'very_inactive' && "ring-2 ring-primary ring-offset-2")}
        />
        <StatCard
          title="从未请求"
          value={activityStatValue(stats?.never_requested, true)}
          subValue={activityStatHint("注册后未使用", true)}
          icon={Users}
          color="gray"
          onClick={() => { setActivityFilter('never'); setPage(1) }}
          className={cn(activityFilter === 'never' && "ring-2 ring-primary ring-offset-2")}
        />
      </div>

      {/* Batch Delete Actions */}
      <Card className="border-orange-200 bg-orange-50 dark:bg-orange-950/20 dark:border-orange-900">
        <CardContent className="p-4">
          <div className="flex flex-col gap-4">
            <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
              <div className="flex items-center gap-3">
                <div className="p-2 bg-orange-100 dark:bg-orange-900 rounded-lg">
                  <AlertTriangle className="h-5 w-5 text-orange-600 dark:text-orange-400" />
                </div>
                <div>
                  <h3 className="font-medium text-orange-800 dark:text-orange-200">批量用户候选预览</h3>
                  <p className="text-sm text-orange-600 dark:text-orange-400">v0.5 仅允许只读预览，禁止旁路批量写；实际处置请在 NewAPI 管理端逐个复核。</p>
                </div>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  className="border-orange-300 text-orange-700 hover:bg-orange-100 hover:text-orange-800 dark:border-orange-800 dark:text-orange-300 dark:hover:bg-orange-900"
                  onClick={() => previewBatchDelete('very_inactive', false)}
                  disabled={batchPreviewing !== null || !stats?.very_inactive_users}
                >
                  {batchPreviewing === 'very_inactive:soft' ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Eye className="h-4 w-4 mr-2" />}
                  预览非常不活跃 ({activityStatValue(stats?.very_inactive_users)})
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  className="border-gray-300 text-gray-700 hover:bg-gray-100 hover:text-gray-900 dark:border-gray-700 dark:text-gray-300 dark:hover:bg-gray-800"
                  onClick={() => previewBatchDelete('never', false)}
                  disabled={batchPreviewing !== null || !stats?.never_requested}
                >
                  {batchPreviewing === 'never:soft' ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Eye className="h-4 w-4 mr-2" />}
                  预览从未请求 ({activityStatValue(stats?.never_requested, true)})
                </Button>
              </div>
            </div>
            {/* 彻底删除区域 */}
            <div className="border-t border-orange-200 dark:border-orange-800 pt-4">
              <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
                <div className="flex items-center gap-3">
                  <div className="p-2 bg-red-100 dark:bg-red-900 rounded-lg">
                    <AlertTriangle className="h-5 w-5 text-red-600 dark:text-red-400" />
                  </div>
                  <div>
                    <h3 className="font-medium text-red-800 dark:text-red-200">硬删除候选（只读）</h3>
                    <p className="text-sm text-red-600 dark:text-red-400">仅展示候选对象，不提供执行按钮，不会修改 NewAPI 或数据库。</p>
                  </div>
                </div>
                <div className="flex flex-wrap gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    className="border-red-300 text-red-700 hover:bg-red-100 hover:text-red-800 dark:border-red-800 dark:text-red-300 dark:hover:bg-red-900"
                    onClick={() => previewBatchDelete('very_inactive', true)}
                    disabled={batchPreviewing !== null || !stats?.very_inactive_users}
                  >
                    {batchPreviewing === 'very_inactive:hard' ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Eye className="h-4 w-4 mr-2" />}
                    预览硬删除候选
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    className="border-red-300 text-red-700 hover:bg-red-100 hover:text-red-800 dark:border-red-800 dark:text-red-300 dark:hover:bg-red-900"
                    onClick={() => previewBatchDelete('never', true)}
                    disabled={batchPreviewing !== null || !stats?.never_requested}
                  >
                    {batchPreviewing === 'never:hard' ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Eye className="h-4 w-4 mr-2" />}
                    预览从未请求候选
                  </Button>
                </div>
              </div>
            </div>
            {/* 清理已注销用户 */}
            {softDeletedCount > 0 && (
              <div className="border-t border-orange-200 dark:border-orange-800 pt-4">
                <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4">
                  <div className="flex items-center gap-3">
                    <div className="p-2 bg-purple-100 dark:bg-purple-900 rounded-lg">
                      <Trash2 className="h-5 w-5 text-purple-600 dark:text-purple-400" />
                    </div>
                    <div>
                      <h3 className="font-medium text-purple-800 dark:text-purple-200">已注销用户候选</h3>
                      <p className="text-sm text-purple-600 dark:text-purple-400">仅预览可清理对象；v0.5 不提供旁路永久清理。</p>
                    </div>
                  </div>
                  <Button
                    variant="outline"
                    size="sm"
                    className="border-purple-300 text-purple-700 hover:bg-purple-100 hover:text-purple-800 dark:border-purple-800 dark:text-purple-300 dark:hover:bg-purple-900"
                    onClick={previewPurgeSoftDeleted}
                    disabled={purgePreviewing}
                  >
                    {purgePreviewing ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <Eye className="h-4 w-4 mr-2" />}
                    预览注销用户 ({softDeletedCount})
                  </Button>
                </div>
              </div>
            )}
          </div>
        </CardContent>
      </Card>

      {/* Search and Filter */}
      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="text-base font-medium flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Search className="w-4 h-4" />
              用户列表
              <span className="ml-2 text-sm font-normal text-muted-foreground">共 {total} 个</span>
            </div>
            <span className="text-xs font-normal text-muted-foreground">
              {loading ? '正在刷新…' : userQueryState === 'stale' ? '旧数据' : userQueryState === 'unavailable' ? '不可用' : '已更新'}
            </span>
          </CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex flex-col sm:flex-row gap-4 mb-4">
            <div className="flex-1 flex gap-2">
              <div className="relative flex-1 max-w-sm">
                <label className="sr-only" htmlFor="user-management-search">搜索用户</label>
                <Search aria-hidden="true" className="absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  id="user-management-search"
                  placeholder="搜索用户名/邮箱/LinuxDoID/邀请码..."
                  value={searchInput}
                  onChange={(e) => setSearchInput(e.target.value)}
                  onKeyPress={handleKeyPress}
                  className="pl-9"
                />
              </div>
              <Button onClick={handleSearch}>搜索</Button>
            </div>
            <div className="w-full sm:w-40">
              <Select aria-label="按活跃度筛选用户" value={activityFilter} onChange={(e) => { setActivityFilter(e.target.value); setPage(1) }}>
                <option value="all">所有状态</option>
                <option value="active">活跃用户</option>
                <option value="inactive">不活跃用户</option>
                <option value="very_inactive">非常不活跃</option>
                <option value="never">从未请求</option>
              </Select>
            </div>
            <div className="w-full sm:w-36">
              <Select aria-label="按分组筛选用户" value={groupFilter} onChange={(e) => { setGroupFilter(e.target.value); setPage(1) }}>
                <option value="">所有分组</option>
                {groups.map((g) => (
                  <option key={g.group_name} value={g.group_name}>
                    {g.group_name === 'default' ? 'default（未分组）' : g.group_name}
                  </option>
                ))}
              </Select>
            </div>
            <div className="w-full sm:w-36">
              <Select aria-label="按注册来源筛选用户" value={sourceFilter} onChange={(e) => { setSourceFilter(e.target.value); setPage(1) }}>
                <option value="">所有来源</option>
                {Object.entries(SOURCE_LABELS).map(([key, info]) => (
                  <option key={key} value={key}>{info.label}</option>
                ))}
              </Select>
            </div>
          </div>

          {(search || activityFilter !== 'all' || groupFilter || sourceFilter) && (
            <div className="mb-4 flex flex-wrap items-center gap-2" aria-label="已应用的用户筛选">
              <span className="text-xs text-muted-foreground">已应用：</span>
              {search && (
                <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => { setSearch(''); setSearchInput(''); setPage(1) }}>
                  搜索：{search} ×
                </Button>
              )}
              {activityFilter !== 'all' && (
                <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => { setActivityFilter('all'); setPage(1) }}>
                  活跃度：{activityFilter} ×
                </Button>
              )}
              {groupFilter && (
                <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => { setGroupFilter(''); setPage(1) }}>
                  分组：{groupFilter} ×
                </Button>
              )}
              {sourceFilter && (
                <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => { setSourceFilter(''); setPage(1) }}>
                  来源：{SOURCE_LABELS[sourceFilter]?.label || sourceFilter} ×
                </Button>
              )}
              <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={() => {
                setSearch('')
                setSearchInput('')
                setActivityFilter('all')
                setGroupFilter('')
                setSourceFilter('')
                setPage(1)
              }}>
                清除全部
              </Button>
            </div>
          )}

          {loading && users.length > 0 && (
            <div className="mb-4 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-800 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-200">
              正在加载新条件；当前旧行已锁定，不能打开详情或执行操作。
            </div>
          )}
          {userQueryState === 'stale' && (
            <div role="status" className="mb-4 rounded-md border border-orange-300 bg-orange-50 px-3 py-2 text-xs text-orange-800 dark:border-orange-800 dark:bg-orange-950/30 dark:text-orange-200">
              新查询不可用。下方仅保留“{displayedQuery}”的旧数据，已禁止操作。{userQueryError ? ` ${userQueryError}` : ''}
            </div>
          )}
          {userQueryState === 'unavailable' && (
            <div role="alert" className="mb-4 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-3 text-sm text-destructive">
              用户列表当前不可用；这不是空结果。{userQueryError ? ` ${userQueryError}` : ''}
            </div>
          )}

          {/* Batch Move */}
          {users.length > 0 && (
            <div className="flex flex-col sm:flex-row sm:items-center gap-3 mb-4 p-3 rounded-lg border bg-muted/20">
              <div className="flex flex-wrap items-center gap-2 text-sm">
                <span className="text-muted-foreground">
                  已选择 <span className="font-medium text-foreground">{selectedUserIds.size}</span> 个
                </span>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-8"
                  onClick={toggleSelectAllOnPage}
                  disabled={listInteractionDisabled}
                >
                  {allSelectedOnPage ? '取消全选本页' : '全选本页'}
                </Button>
                {selectedUserIds.size > 0 && (
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-8"
                    onClick={() => setSelectedUserIds(new Set())}
                  >
                    清空
                  </Button>
                )}
              </div>

              <div className="w-full sm:w-auto sm:ml-auto text-xs text-muted-foreground">
                v0.5 仅保留选择与影响范围预览；批量分组写入请在 NewAPI 管理端复核执行。
              </div>
            </div>
          )}

          {/* Users Table */}
          {loading && !users.length ? (
            <div className="flex justify-center py-12">
              <Loader2 aria-hidden="true" className="h-8 w-8 animate-spin text-primary motion-reduce:animate-none" />
            </div>
          ) : users.length > 0 ? (
            <div className="max-w-full overflow-x-auto rounded-md border">
              <Table>
                <TableHeader className="bg-muted/50">
                  <TableRow>
                    <TableHead className="w-10">
                      <input
                        type="checkbox"
                        role="checkbox"
                        aria-label="全选本页用户"
                        checked={allSelectedOnPage}
                        onChange={toggleSelectAllOnPage}
                        disabled={listInteractionDisabled}
                        className="h-4 w-4 rounded border-input text-primary focus-visible:ring-2 focus-visible:ring-ring"
                      />
                    </TableHead>
                    <TableHead className="w-16">ID</TableHead>
                    <TableHead>用户</TableHead>
                    <TableHead className="hidden sm:table-cell">角色</TableHead>
                    <TableHead>状态</TableHead>
                    <TableHead className="hidden lg:table-cell">Linux.do</TableHead>
                    <TableHead className="text-right">额度 (USD)</TableHead>
                    <TableHead className="text-right hidden sm:table-cell">已用</TableHead>
                    <TableHead className="text-right hidden md:table-cell">请求数</TableHead>
                    <TableHead className="hidden md:table-cell">最后请求</TableHead>
                    <TableHead>活跃度</TableHead>
                    <TableHead className="w-20">操作</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {users.map((user) => (
                    <TableRow key={user.id} className="hover:bg-muted/50 transition-colors motion-reduce:transition-none group">
                      <TableCell className="w-10">
                        <input
                          type="checkbox"
                          role="checkbox"
                          aria-label={`选择用户 ${user.username}`}
                          checked={selectedUserIds.has(user.id)}
                          onChange={() => toggleSelectUser(user.id)}
                          disabled={listInteractionDisabled}
                          className="h-4 w-4 rounded border-input text-primary focus-visible:ring-2 focus-visible:ring-ring"
                        />
                      </TableCell>
                      <TableCell className="font-mono text-xs text-muted-foreground tabular-nums">{user.id}</TableCell>
                      <TableCell>
                        <button
                          type="button"
                          className={cn(
                            "flex items-center gap-3 px-3 py-2 rounded-xl bg-muted/30 transition-all motion-reduce:transition-none border border-transparent w-max min-w-[180px] text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2",
                            listInteractionDisabled ? "cursor-not-allowed opacity-60" : "hover:bg-primary/5 hover:border-primary/20",
                          )}
                          onClick={() => openUserAnalysis(user.id, user.username)}
                          disabled={listInteractionDisabled}
                          aria-label={`打开用户 ${user.username} 详情`}
                          title="查看用户分析"
                        >
                          <span className="w-8 h-8 rounded-full bg-primary/10 flex items-center justify-center border border-primary/20 text-sm text-primary font-bold shrink-0">
                            {user.username[0]?.toUpperCase()}
                          </span>
                          <span className="flex flex-col min-w-0">
                            <span className="font-bold text-sm tracking-tight">{user.username}</span>
                            <span className="flex items-center gap-1.5 mt-0.5">
                              {user.display_name && (
                                <span className="text-[10px] text-muted-foreground">{user.display_name}</span>
                              )}
                              <span className="inline-flex h-4 shrink-0 items-center whitespace-nowrap rounded-full border border-muted-foreground/20 px-1.5 py-0 text-[9px] font-medium leading-none text-foreground">
                                {user.group || 'default'}
                              </span>
                            </span>
                          </span>
                        </button>
                      </TableCell>
                      <TableCell className="hidden sm:table-cell">
                        {getRoleBadge(user.role)}
                      </TableCell>
                      <TableCell>{getStatusBadge(user.status)}</TableCell>
                      <TableCell className="hidden lg:table-cell">
                        {user.linux_do_id ? (
                          <button
                            onClick={async () => {
                              const lid = user.linux_do_id
                              if (!lid || linuxDoLookupLoading) return
                              setLinuxDoLookupLoading(lid)
                              try {
                                const res = await fetch(`${apiUrl}/api/linuxdo/lookup/${encodeURIComponent(lid)}`, { headers: getAuthHeaders() })
                                const data = await res.json()
                                if (data.success && data.data?.profile_url) {
                                  window.open(data.data.profile_url, '_blank')
                                } else if (data.error_type === 'rate_limit') {
                                  showToast('error', data.message || `请求被限速，请等待 ${data.wait_seconds || '?'} 秒后重试`)
                                } else if (data.fallback_url) {
                                  window.open(data.fallback_url, '_blank')
                                  showToast('info', '服务器查询失败，已在新标签页打开 Linux.do 证书页面')
                                } else {
                                  showToast('error', data.message || '查询 Linux.do 用户名失败')
                                }
                              } catch { showToast('error', '查询 Linux.do 用户名失败') }
                              finally { setLinuxDoLookupLoading(null) }
                            }}
                            disabled={listInteractionDisabled || linuxDoLookupLoading === user.linux_do_id}
                            className="text-xs font-mono text-blue-500 hover:text-blue-600 hover:underline disabled:opacity-50 cursor-pointer"
                            title="点击查看 Linux.do 用户主页"
                          >
                            {linuxDoLookupLoading === user.linux_do_id ? '查询中...' : user.linux_do_id}
                          </button>
                        ) : (
                          <span className="text-xs text-muted-foreground">-</span>
                        )}
                      </TableCell>
                      <TableCell className="text-right font-mono text-sm font-bold text-primary tabular-nums tracking-tight">
                        {formatQuota(user.quota)}
                      </TableCell>
                      <TableCell className="text-right font-mono text-xs text-muted-foreground hidden sm:table-cell tabular-nums">
                        {formatQuota(user.used_quota)}
                      </TableCell>
                      <TableCell className="text-right hidden md:table-cell tabular-nums font-bold text-sm">
                        {user.request_count.toLocaleString()}
                      </TableCell>
                      <TableCell className="hidden md:table-cell text-xs whitespace-nowrap tabular-nums text-muted-foreground">{formatLastRequest(user)}</TableCell>
                      <TableCell>{getActivityBadge(user.activity_level)}</TableCell>
                      <TableCell>
                        <div className="flex items-center gap-0.5 opacity-100 transition-opacity motion-reduce:transition-none sm:opacity-0 sm:group-hover:opacity-100 sm:focus-within:opacity-100">
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            className="text-blue-500 hover:text-blue-600 hover:bg-blue-500/10 h-7 w-7 p-0"
                            onClick={() => openUserAnalysis(user.id, user.username)}
                            disabled={listInteractionDisabled}
                            aria-label={`查看用户 ${user.username} 的分析`}
                            title="用户分析"
                          >
                            <Eye aria-hidden="true" className="h-3.5 w-3.5" />
                          </Button>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            className="text-muted-foreground hover:text-destructive hover:bg-destructive/10 h-7 w-7 p-0"
                            onClick={() => deleteUser(user.id, user.username)}
                            disabled={deleting || listInteractionDisabled}
                            aria-label={`删除用户 ${user.username}`}
                            title="删除用户"
                          >
                            <Trash2 aria-hidden="true" className="h-3.5 w-3.5" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          ) : (
            <div className="py-20 text-center text-muted-foreground bg-muted/10 rounded-lg border border-dashed">
              <Users className="mx-auto h-10 w-10 mb-3 opacity-20" />
              <p>{userQueryState === 'unavailable'
                ? '用户列表不可用，请稍后重试'
                : userQueryState === 'empty'
                  ? (search || activityFilter !== 'all' || groupFilter || sourceFilter ? '当前条件结果为空' : '暂无用户数据')
                  : '暂无可显示的用户数据'}</p>
            </div>
          )}

          {/* Pagination */}
          {totalPages > 1 && (
            <div className="flex items-center justify-between mt-4 px-2">
              <p className="text-sm text-muted-foreground">
                第 {page} / {totalPages} 页
              </p>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setPage(p => Math.max(1, p - 1))}
                  disabled={page === 1 || loading}
                >
                  <ChevronLeft className="h-4 w-4 mr-1" />
                  上一页
                </Button>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setPage(p => Math.min(totalPages, p + 1))}
                  disabled={page === totalPages || loading}
                >
                  下一页
                  <ChevronRight className="h-4 w-4 ml-1" />
                </Button>
              </div>
            </div>
          )}
        </CardContent>
      </Card>

      {/* Confirm Dialog */}
      <Dialog open={confirmDialog.isOpen} onOpenChange={(open: boolean) => {
        if (!open && deleteUserTarget) {
          closeDeleteDialog()
          return
        }
        setConfirmDialog(prev => ({ ...prev, isOpen: open }))
        if (!open) {
          setDeleteConfirmText('')
          setDeleteReason('')
        }
      }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className={confirmDialog.hardDelete || (deleteUserTarget !== null && deleteMode === 'hard') ? "text-red-600 dark:text-red-400" : ""}>{confirmDialog.title}</DialogTitle>
            <DialogDescription className="whitespace-pre-line">{confirmDialog.message}</DialogDescription>
          </DialogHeader>
          {confirmDialog.loading ? (
            <div className="py-8 flex flex-col items-center justify-center">
              <Loader2 className="h-8 w-8 animate-spin text-primary mb-3" />
              <p className="text-sm text-muted-foreground">正在查询用户数据，请等待预览结果...</p>
            </div>
          ) : deleteUserTarget ? (
            /* 单个用户删除 - 显示模式选择 */
            <div className="py-4 space-y-4">
              {deletePayloadLocked && (
                <div className="flex items-start gap-2 rounded-md border border-yellow-500/30 bg-yellow-500/10 p-3 text-sm text-muted-foreground">
                  <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0 text-yellow-600" />
                  <span>
                    该用户已有持久化操作锁。浏览器不会盲目重试，只能读取 Tool Store 的明确终态后解锁。
                    {deleteReleaseCandidate && (
                      <span className="block mt-1 font-medium text-amber-700 dark:text-amber-300">
                        审计 #{deleteReleaseCandidate.audit_id} 已确认为 {deleteReleaseCandidate.status}。请显式确认后解除本地锁。
                      </span>
                    )}
                  </span>
                </div>
              )}
              <div className="text-sm text-muted-foreground">
                用户: <span className="font-medium text-foreground">{deleteUserTarget.username}</span>
              </div>
              <div className="space-y-3">
                <label className={`flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${deleteMode === 'soft' ? 'border-primary bg-primary/5' : 'border-border hover:border-primary/50'}`}>
                  <input
                    type="radio"
                    name="deleteMode"
                    checked={deleteMode === 'soft'}
                    onChange={() => { setDeleteMode('soft'); setDeleteConfirmText('') }}
                    disabled={deletePayloadLocked}
                    className="mt-1"
                  />
                  <div>
                    <div className="font-medium">注销用户</div>
                    <div className="text-sm text-muted-foreground">数据保留，可通过数据库恢复。用户名仍被占用。</div>
                  </div>
                </label>
                <label className={cn(
                  'flex items-start gap-3 p-3 rounded-lg border transition-colors',
                  hardDeleteAvailable
                    ? 'cursor-pointer hover:border-red-300'
                    : 'cursor-not-allowed opacity-60',
                  deleteMode === 'hard' && hardDeleteAvailable
                    ? 'border-red-500 bg-red-50 dark:bg-red-950/20'
                    : 'border-border',
                )}>
                  <input
                    type="radio"
                    name="deleteMode"
                    checked={deleteMode === 'hard'}
                    onChange={() => { setDeleteMode('hard'); setDeleteConfirmText('') }}
                    disabled={!hardDeleteAvailable || deletePayloadLocked}
                    className="mt-1"
                  />
                  <div>
                    <div className="font-medium text-red-600 dark:text-red-400">兼容硬删除（仅管理员）</div>
                    <div className="text-sm text-muted-foreground">
                      {hardDeleteAvailable
                        ? '能力探测已确认当前版本、管理员 API 与凭据均可用；仍可能遗留 2FA、Passkey、OAuth 绑定，请优先使用 NewAPI 后台。'
                        : capabilitiesLoading
                          ? '正在探测 NewAPI 安全硬删除能力，确认前保持关闭。'
                          : capabilitiesError
                            ? '能力探测失败，已按只读保护关闭硬删除。'
                            : '当前 NewAPI 版本或写入能力未确认安全硬删除，已保持关闭。'}
                    </div>
                  </div>
                </label>
              </div>
              <div className="border-t pt-4">
                <p className="text-sm font-medium mb-2">操作原因（必填，至少 3 个字符）：</p>
                <Input
                  value={deleteReason}
                  onChange={(e) => setDeleteReason(e.target.value)}
                  placeholder="例如：用户主动申请注销"
                  maxLength={1000}
                  disabled={deletePayloadLocked}
                />
              </div>
              <div className="border-t pt-4">
                <p className={cn(
                  "text-sm font-medium mb-2",
                  deleteMode === 'hard' ? "text-red-600 dark:text-red-400" : "text-orange-600 dark:text-orange-400"
                )}>
                  请输入 <span className="font-mono bg-red-100 dark:bg-red-900 px-2 py-0.5 rounded">{currentDialogConfirmText}</span> 以确认操作：
                </p>
                <Input
                  value={deleteConfirmText}
                  onChange={(e) => setDeleteConfirmText(e.target.value)}
                  placeholder={`请输入 ${currentDialogConfirmText}`}
                  className={deleteMode === 'hard' ? "border-red-300 focus:border-red-500 focus:ring-red-500" : "border-orange-300 focus:border-orange-500 focus:ring-orange-500"}
                  disabled={deletePayloadLocked}
                />
              </div>
            </div>
          ) : confirmDialog.details && (
            /* 批量删除 - 显示用户列表 */
            <div className="py-4 space-y-4">
              <div>
                <p className="text-sm text-muted-foreground mb-2">
                  {confirmDialog.previewOnly
                    ? '以下用户仅供只读预览（显示前20个）：'
                    : `将${confirmDialog.hardDelete ? '彻底删除' : '注销'}以下用户（显示前20个）：`}
                </p>
                <div className="max-h-40 overflow-y-auto bg-muted rounded-md p-3">
                  <div className="flex flex-wrap gap-2">
                    {confirmDialog.details.users.map((username, i) => (
                      <Badge key={i} variant="outline">{username}</Badge>
                    ))}
                    {confirmDialog.details.count > 20 && (
                      <Badge variant="secondary">+{confirmDialog.details.count - 20} 更多</Badge>
                    )}
                  </div>
                </div>
              </div>
              {/* 所有批量删除/清理操作都需要输入确认 */}
              {confirmDialog.requireConfirmText && (
                <div className="border-t pt-4">
                  <p className={cn(
                    "text-sm font-medium mb-2",
                    confirmDialog.hardDelete ? "text-red-600 dark:text-red-400" : "text-orange-600 dark:text-orange-400"
                  )}>
                    请输入 <span className="font-mono bg-red-100 dark:bg-red-900 px-2 py-0.5 rounded">{currentDialogConfirmText}</span> 以确认操作：
                  </p>
                  <Input
                    value={deleteConfirmText}
                    onChange={(e) => setDeleteConfirmText(e.target.value)}
                    placeholder={`请输入 ${currentDialogConfirmText}`}
                    className={confirmDialog.hardDelete ? "border-red-300 focus:border-red-500 focus:ring-red-500" : "border-orange-300 focus:border-orange-500 focus:ring-orange-500"}
                  />
                </div>
              )}
            </div>
          )}
          <DialogFooter>
            {confirmDialog.previewOnly ? (
              <Button onClick={() => setConfirmDialog(prev => ({ ...prev, isOpen: false }))}>
                关闭预览
              </Button>
            ) : (
              <>
                <Button variant="outline" onClick={deleteUserTarget ? closeDeleteDialog : () => { setConfirmDialog(prev => ({ ...prev, isOpen: false })); setDeleteConfirmText(''); setDeleteReason('') }} disabled={deleteReconciling}>
                  {deletePayloadLocked ? '暂时关闭' : '取消'}
                </Button>
                {deleteReconciliationRequired && deleteUserTarget ? (
                  <Button onClick={reconcileDeleteUser} disabled={deleting || deleteReconciling}>
                    {deleteReconciling ? <Loader2 className="h-4 w-4 mr-2 animate-spin" /> : <RefreshCw className="h-4 w-4 mr-2" />}
                    {deleteReleaseCandidate ? '确认解除本地锁' : '重新对账'}
                  </Button>
                ) : (
                  <Button
                    variant={confirmDialog.type === 'danger' || (deleteUserTarget !== null && deleteMode === 'hard') ? 'destructive' : 'default'}
                    onClick={() => {
                      if (deleteUserTarget) {
                        executeDeleteUser()
                      } else {
                        confirmDialog.onConfirm()
                      }
                    }}
                    disabled={confirmActionDisabled}
                  >
                    {deleting
                      ? '正在提交...'
                      : deleteUserTarget
                        ? deleteMode === 'hard' ? '确认彻底删除' : '确认注销'
                        : (confirmDialog.hardDelete ? '确认彻底删除' : '确认注销')}
                  </Button>
                )}
              </>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* User Analysis Dialog */}
      {selectedUser && (
        <UserAnalysisDialog
          open={analysisDialogOpen}
          onOpenChange={handleAnalysisOpenChange}
          userId={selectedUser.id}
          username={selectedUser.username}
          source="user_management"
          onBanned={() => fetchUsers()}
          onUnbanned={() => fetchUsers()}
          onWhitelistChanged={() => fetchUsers()}
          renderExtra={() => (
            <div className="space-y-3">
              <h4 className="text-sm font-semibold text-muted-foreground flex items-center gap-2">
                <Users className="w-4 h-4" />
                邀请用户
                {invitedUsers?.inviter?.aff_code && (
                  <Badge variant="outline" className="text-xs px-1.5 py-0 font-mono">
                    邀请码: {invitedUsers.inviter.aff_code}
                  </Badge>
                )}
                {invitedUsers?.stats && invitedUsers.stats.total_invited > 0 && (
                  <Badge variant="secondary" className="text-xs px-1.5 py-0">
                    共 {invitedUsers.stats.total_invited} 人
                  </Badge>
                )}
              </h4>

              {invitedState === 'stale' && (
                <div role="status" className="rounded-md border border-orange-300 bg-orange-50 px-3 py-2 text-xs text-orange-800 dark:border-orange-800 dark:bg-orange-950/30 dark:text-orange-200">
                  邀请详情刷新失败；下方为旧数据，已禁止翻页。{invitedError ? ` ${invitedError}` : ''}
                </div>
              )}
              {invitedState === 'unavailable' ? (
                <div role="alert" className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-3 text-xs text-destructive">
                  邀请详情当前不可用；这不是“无邀请记录”。{invitedError ? ` ${invitedError}` : ''}
                </div>
              ) : invitedLoading ? (
                <div className="flex items-center justify-center py-6">
                  <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
                </div>
              ) : invitedUsers?.items && invitedUsers.items.length > 0 ? (
                <>
                  {/* 邀请统计 */}
                  <div className="grid grid-cols-4 gap-2">
                    <div className="rounded-lg border bg-muted/30 p-2 text-center">
                      <div className="text-sm font-bold">{invitedUsers.stats.total_invited}</div>
                      <div className="text-xs text-muted-foreground">邀请总数</div>
                    </div>
                    <div className="rounded-lg border bg-green-50 dark:bg-green-900/20 p-2 text-center">
                      <div className="text-sm font-bold text-green-600">{invitedUsers.stats.requested_count}</div>
                      <div className="text-xs text-muted-foreground">有过请求</div>
                    </div>
                    <div className={cn(
                      "rounded-lg border p-2 text-center",
                      invitedUsers.stats.banned_count > 0 ? "bg-red-50 dark:bg-red-900/20" : "bg-muted/30"
                    )}>
                      <div className={cn("text-sm font-bold", invitedUsers.stats.banned_count > 0 && "text-red-600")}>{invitedUsers.stats.banned_count}</div>
                      <div className="text-xs text-muted-foreground">已封禁</div>
                    </div>
                    <div className="rounded-lg border bg-muted/30 p-2 text-center">
                      <div className="text-sm font-bold">{invitedUsers.stats.total_used_quota === undefined ? '—' : (invitedUsers.stats.total_used_quota / 500000).toFixed(2)}</div>
                      <div className="text-xs text-muted-foreground">总消耗 $</div>
                    </div>
                  </div>

                  {/* 邀请用户列表 */}
                  <div className="rounded-lg border overflow-hidden">
                    <Table>
                      <TableHeader>
                        <TableRow className="h-8 bg-muted/50 hover:bg-muted/50">
                          <TableHead className="h-8 text-xs w-[60px]">ID</TableHead>
                          <TableHead className="h-8 text-xs">用户名</TableHead>
                          <TableHead className="h-8 text-xs w-[60px]">状态</TableHead>
                          <TableHead className="h-8 text-xs text-right">请求数</TableHead>
                          <TableHead className="h-8 text-xs text-right">消耗 $</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {invitedUsers.items.map((u) => (
                          <TableRow key={u.user_id} className="h-8 hover:bg-muted/30">
                            <TableCell className="py-1.5 text-xs text-muted-foreground font-mono">{u.user_id}</TableCell>
                            <TableCell className="py-1.5 text-xs">
                              <span className="font-medium">{u.username}</span>
                              {u.display_name && <span className="text-muted-foreground ml-1">({u.display_name})</span>}
                            </TableCell>
                            <TableCell className="py-1.5 text-xs">
                              {u.status === 2 ? (
                                <Badge variant="destructive" className="text-xs px-1 py-0">禁用</Badge>
                              ) : (
                                <Badge variant="success" className="text-xs px-1 py-0">正常</Badge>
                              )}
                            </TableCell>
                            <TableCell className="py-1.5 text-xs text-right tabular-nums">{u.request_count.toLocaleString()}</TableCell>
                            <TableCell className="py-1.5 text-xs text-right tabular-nums font-mono">{u.used_quota === undefined ? '—' : (u.used_quota / 500000).toFixed(2)}</TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                  </div>

                  {/* 分页 */}
                  {invitedUsers.total > INVITED_USERS_PAGE_SIZE && (
                    <div className="flex items-center justify-between pt-2">
                      <span className="text-xs text-muted-foreground">
                        第 {invitedPage} 页，共 {Math.ceil(invitedUsers.total / INVITED_USERS_PAGE_SIZE)} 页
                      </span>
                      <div className="flex gap-1">
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          className="h-7 px-2 text-xs"
                          onClick={() => setInvitedPage(p => Math.max(1, p - 1))}
                          disabled={invitedPage === 1 || invitedLoading || invitedState === 'stale'}
                          aria-label="邀请用户上一页"
                        >
                          <ChevronLeft aria-hidden="true" className="h-3 w-3" />
                        </Button>
                        <Button
                          type="button"
                          variant="outline"
                          size="sm"
                          className="h-7 px-2 text-xs"
                          onClick={() => setInvitedPage(p => p + 1)}
                          disabled={invitedPage >= Math.ceil(invitedUsers.total / INVITED_USERS_PAGE_SIZE) || invitedLoading || invitedState === 'stale'}
                          aria-label="邀请用户下一页"
                        >
                          <ChevronRight aria-hidden="true" className="h-3 w-3" />
                        </Button>
                      </div>
                    </div>
                  )}
                </>
              ) : (
                <div className="text-xs text-muted-foreground italic py-4 text-center border rounded-lg bg-muted/10">
                  该用户暂无邀请记录
                </div>
              )}
            </div>
          )}
        />
      )}
        </TabsContent>

        <TabsContent value="affiliate" className="mt-6">
          <AffiliateStats />
        </TabsContent>
      </Tabs>
    </div>
  )
}
