import { useState, useEffect, useCallback, useRef } from 'react'
import { fetchApi } from '@/shared/api/client'
import { toast } from 'sonner'
import type {
  UsageLog,
  UsageStats,
  ApiKey,
  TooltipData,
} from '@/features/user-usage/types'
import { UsageStatsCards } from '@/features/user-usage/components/UsageStatsCards'
import { UsageFilterBar } from '@/features/user-usage/components/UsageFilterBar'
import { UsageTable } from '@/features/user-usage/components/UsageTable'
import { UsageCostTooltip } from '@/features/user-usage/components/UsageCostTooltip'
import { UsageTokenTooltip } from '@/features/user-usage/components/UsageTokenTooltip'

// ── Helpers ─────────────────────────────────────────────────────────────────

function todayStr(): string {
  const d = new Date()
  return `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')}`
}

function daysAgo(n: number): string {
  const d = new Date(Date.now() - n * 86400000)
  return `${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')}`
}

function fmtDateTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

// ── Page Component ──────────────────────────────────────────────────────────

export default function Usage() {
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [stats, setStats] = useState<UsageStats | null>(null)
  const [loading, setLoading] = useState(true)
  const [exporting, setExporting] = useState(false)

  // Filters
  const [apiKeys, setApiKeys] = useState<ApiKey[]>([])
  const [filterKeyId, setFilterKeyId] = useState<string>('')
  const [filterModel, setFilterModel] = useState('')
  const [dateRange, setDateRange] = useState<'today' | '7d' | '30d' | 'custom'>('7d')
  const [startDate, setStartDate] = useState(daysAgo(6))
  const [endDate, setEndDate] = useState(todayStr())

  // Pagination
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [total, setTotal] = useState(0)

  // Tooltips
  const [costTooltip, setCostTooltip] = useState<TooltipData | null>(null)
  const [tokenTooltip, setTokenTooltip] = useState<TooltipData | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)

  // Load API keys for filter
  useEffect(() => {
    fetchApi('/user/api-keys').then(res => {
      if (res?.data) {
        const keys = (Array.isArray(res.data) ? res.data : res.data.items || []) as ApiKey[]
        setApiKeys(keys.map(k => ({ id: k.id, name: k.name })))
      }
    }).catch(() => {})
  }, [])

  // Build query params
  const buildParams = useCallback((p: number, ps: number) => {
    const params = new URLSearchParams()
    params.set('page', String(p))
    params.set('page_size', String(ps))
    if (filterKeyId) params.set('api_key_id', filterKeyId)
    if (filterModel.trim()) params.set('model', filterModel.trim())

    let sd = startDate, ed = endDate
    if (dateRange === 'today') { sd = todayStr(); ed = todayStr() }
    else if (dateRange === '7d') { sd = daysAgo(6); ed = todayStr() }
    else if (dateRange === '30d') { sd = daysAgo(29); ed = todayStr() }
    if (sd) params.set('start_date', sd)
    if (ed) params.set('end_date', ed)

    return params.toString()
  }, [filterKeyId, filterModel, dateRange, startDate, endDate])

  const loadData = useCallback(async (p = page, ps = pageSize) => {
    setLoading(true)
    try {
      const qs = buildParams(p, ps)
      const res = await fetchApi(`/user/usage/detail?${qs}`)
      if (res?.data) {
        setLogs(res.data.items || [])
        setTotal(res.data.total || 0)
        setStats(res.data.stats || null)
        setPage(res.data.page || p)
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '加载使用数据失败')
    } finally {
      setLoading(false)
    }
  }, [buildParams, page, pageSize])

  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => { loadData(1) }, [])

  const handleFilter = () => { setPage(1); loadData(1) }
  const handlePageChange = (newPage: number) => { setPage(newPage); loadData(newPage) }
  const handlePageSizeChange = (newSize: number) => { setPageSize(newSize); setPage(1); loadData(1, newSize) }

  const totalPages = Math.ceil(total / pageSize)

  // CSV Export
  const handleExport = async () => {
    if (total === 0) { toast.warning('当前筛选条件下没有数据可导出'); return }
    setExporting(true)
    toast.info('正在准备导出...')
    try {
      const allLogs: UsageLog[] = []
      const ps = 100
      const pages = Math.ceil(total / ps)
      for (let p = 1; p <= pages; p++) {
        const qs = buildParams(p, ps)
        const res = await fetchApi(`/user/usage/detail?${qs}`)
        if (res?.data?.items) allLogs.push(...res.data.items)
      }

      const header = '时间,模型,API Key,类型,输入Tokens,输出Tokens,推理Tokens,缓存Tokens,标准费用,实际扣费,倍率,耗时(ms),状态\n'
      const rows = allLogs.map(l =>
        [
          fmtDateTime(l.created_at),
          l.model,
          l.api_key_name || '-',
          l.stream ? 'Stream' : 'Sync',
          l.input_tokens,
          l.output_tokens,
          l.reasoning_tokens,
          l.cached_tokens,
          l.total_cost.toFixed(6),
          l.actual_cost.toFixed(6),
          l.rate_multiplier,
          l.duration_ms ?? '',
          l.failed ? '失败' : '成功',
        ].join(',')
      ).join('\n')

      const BOM = '﻿'
      const blob = new Blob([BOM + header + rows], { type: 'text/csv;charset=utf-8;' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = `usage_${startDate}_${endDate}.csv`
      a.click()
      URL.revokeObjectURL(url)
      toast.success(`导出成功，共 ${allLogs.length} 条记录`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '导出失败')
    } finally {
      setExporting(false)
    }
  }

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500" style={{ willChange: 'transform, opacity' }} ref={containerRef}>
      {/* Header */}
      <div>
        <h2 className="text-2xl font-bold tracking-tight text-gray-900 dark:text-white">使用明细</h2>
        <p className="text-gray-500 dark:text-dark-300 mt-1">查看每一次 API 调用的详细信息、Token 用量及费用。</p>
      </div>

      <UsageStatsCards stats={stats} />

      <UsageFilterBar
        apiKeys={apiKeys}
        filterKeyId={filterKeyId}
        onFilterKeyIdChange={setFilterKeyId}
        filterModel={filterModel}
        onFilterModelChange={setFilterModel}
        dateRange={dateRange}
        onDateRangeChange={setDateRange}
        startDate={startDate}
        onStartDateChange={setStartDate}
        endDate={endDate}
        onEndDateChange={setEndDate}
        onFilter={handleFilter}
        onRefresh={() => loadData(1)}
        onExport={handleExport}
        loading={loading}
        exporting={exporting}
        total={total}
      />

      <UsageTable
        logs={logs}
        loading={loading}
        total={total}
        page={page}
        pageSize={pageSize}
        totalPages={totalPages}
        onPageChange={handlePageChange}
        onPageSizeChange={handlePageSizeChange}
        onCostTooltip={setCostTooltip}
        onTokenTooltip={setTokenTooltip}
      />

      {costTooltip && <UsageCostTooltip data={costTooltip} onClose={() => setCostTooltip(null)} />}
      {tokenTooltip && <UsageTokenTooltip data={tokenTooltip} onClose={() => setTokenTooltip(null)} />}
    </div>
  )
}
