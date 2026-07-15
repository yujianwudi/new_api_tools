import { useState, useEffect, useCallback } from 'react'
import { useToast } from './Toast'
import * as db from '../services/indexedDB'
import { Clock, Trash2, Loader2, ShieldCheck } from 'lucide-react'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from './ui/card'
import { Button } from './ui/button'
import { Badge } from './ui/badge'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from './ui/dialog'

export interface HistoryItem {
  id: string
  timestamp: string
  name: string
  count: number
  quota_mode: 'fixed' | 'random'
  expire_mode: 'never' | 'days' | 'date'
}

const MAX_HISTORY_ITEMS = 100

export async function addHistoryItem(item: HistoryItem): Promise<void> {
  try {
    await db.addHistoryRecord({
      name: item.name,
      quota: 0,
      count: item.count,
      quota_mode: item.quota_mode,
      expire_mode: item.expire_mode,
    })
  } catch (error) {
    console.error('Failed to add history item:', error)
  }
}

export async function clearHistory(): Promise<void> {
  await db.clearHistory()
}

export function History() {
  const { showToast } = useToast()
  const [historyItems, setHistoryItems] = useState<HistoryItem[]>([])
  const [loading, setLoading] = useState(true)
  const [clearDialog, setClearDialog] = useState(false)

  const loadHistory = useCallback(async () => {
    try {
      setLoading(true)
      const records = await db.getHistoryRecords(MAX_HISTORY_ITEMS)
      const items: HistoryItem[] = records.map(record => ({
        id: record.id,
        timestamp: new Date(record.timestamp).toISOString(),
        name: record.name,
        count: record.count,
        quota_mode: record.quota_mode,
        expire_mode: record.expire_mode,
      }))
      setHistoryItems(items)
    } catch (error) {
      console.error('Failed to load history:', error)
      showToast('error', '加载历史记录失败')
    } finally {
      setLoading(false)
    }
  }, [showToast])

  useEffect(() => { loadHistory() }, [loadHistory])

  const handleDelete = async (id: string) => {
    try {
      await db.deleteHistoryRecord(id)
      setHistoryItems(prev => prev.filter(item => item.id !== id))
      showToast('success', '记录已删除')
    } catch (error) {
      console.error('Failed to delete history item:', error)
      showToast('error', '删除失败')
    }
  }

  const handleClearAll = async () => {
    try {
      await db.clearHistory()
      setHistoryItems([])
      showToast('success', '历史记录已清空')
    } catch (error) {
      console.error('Failed to clear history:', error)
      showToast('error', '清空失败')
    } finally {
      setClearDialog(false)
    }
  }

  const formatDate = (isoString: string) => new Date(isoString).toLocaleString('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
  const getQuotaModeLabel = (mode: string) => mode === 'fixed' ? '固定额度' : '随机额度'
  const getExpireModeLabel = (mode: string) => mode === 'never' ? '永不过期' : mode === 'days' ? '按天数' : '指定日期'

  return (
    <div className="space-y-6 animate-in fade-in duration-500">
      {/* Header */}
      <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
        <div>
          <h2 className="text-3xl font-bold tracking-tight">生成记录</h2>
          <p className="text-muted-foreground mt-1">本地仅保存不含兑换码的生成摘要</p>
        </div>
        {historyItems.length > 0 && (
          <Button variant="destructive" size="sm" onClick={() => setClearDialog(true)}>
            <Trash2 className="h-4 w-4 mr-2" />
            清空记录
          </Button>
        )}
      </div>

      <Card>
        <CardHeader className="pb-4">
          <CardTitle className="text-lg flex items-center gap-2">
            <Clock className="w-5 h-5 text-primary" />
            历史列表
          </CardTitle>
          <CardDescription>
            这里显示最近生成的 {MAX_HISTORY_ITEMS} 条摘要记录，兑换码明文不会写入浏览器存储。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-start gap-3 rounded-lg border border-emerald-500/30 bg-emerald-500/5 p-4 text-sm">
            <ShieldCheck className="mt-0.5 h-5 w-5 shrink-0 text-emerald-600" />
            <p className="text-muted-foreground">
              出于安全原因，兑换码只在生成结果弹窗中即时显示。请在关闭弹窗前复制或下载；之后可在“兑换码”页查询服务端记录。
            </p>
          </div>
          {loading ? (
            <div className="flex justify-center items-center py-12">
              <Loader2 className="h-8 w-8 animate-spin text-primary" />
            </div>
          ) : historyItems.length === 0 ? (
            <div className="text-center py-12 bg-muted/20 rounded-lg border border-dashed">
              <Clock className="mx-auto h-10 w-10 text-muted-foreground opacity-50" />
              <h3 className="mt-4 text-sm font-medium">暂无历史记录</h3>
              <p className="mt-1 text-sm text-muted-foreground">生成后会在这里保存不含兑换码的摘要</p>
            </div>
          ) : (
            <div className="space-y-3">
              {historyItems.map((item) => (
                <div key={item.id} className="border rounded-lg overflow-hidden transition-all duration-200 hover:shadow-md bg-card">
                  <div className="flex items-center justify-between gap-4 p-4">
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-3">
                        <h3 className="text-sm font-medium truncate">{item.name}</h3>
                        <Badge variant="secondary" className="text-xs font-normal">{item.count} 个</Badge>
                      </div>
                      <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
                        <span className="flex items-center gap-1">
                          <Clock className="w-3 h-3" />
                          {formatDate(item.timestamp)}
                        </span>
                        <span className="hidden sm:inline">•</span>
                        <span>{getQuotaModeLabel(item.quota_mode)}</span>
                        <span className="hidden sm:inline">•</span>
                        <span>{getExpireModeLabel(item.expire_mode)}</span>
                      </div>
                    </div>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 shrink-0 text-destructive hover:text-destructive hover:bg-destructive/10"
                      onClick={() => handleDelete(item.id)}
                      title="删除摘要"
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </div>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Dialog open={clearDialog} onOpenChange={setClearDialog}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>确认清空</DialogTitle>
            <DialogDescription>确定要清空所有历史记录吗？此操作不可恢复。</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setClearDialog(false)}>取消</Button>
            <Button variant="destructive" onClick={handleClearAll}>确认清空</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
