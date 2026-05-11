import { FileText, RefreshCw, ChevronLeft, ChevronRight, ArrowDownCircle, ArrowUpCircle, CheckCircle2, XCircle, Info } from 'lucide-react'
import type { UsageTableProps } from '../types'

function fmtDuration(ms: number | null): string {
  if (ms == null) return '-'
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

function fmtTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(2)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
  return n.toLocaleString()
}

function fmtCost(n: number): string {
  if (n === 0) return '$0.00'
  if (n < 0.0001) return `$${n.toFixed(6)}`
  if (n < 0.01) return `$${n.toFixed(4)}`
  return `$${n.toFixed(4)}`
}

function fmtDateTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export function UsageTable({
  logs,
  loading,
  total,
  page,
  pageSize,
  totalPages,
  onPageChange,
  onPageSizeChange,
  onCostTooltip,
  onTokenTooltip,
}: UsageTableProps) {
  return (
    <div className="glass-card overflow-hidden">
      <div className="overflow-x-auto">
        <table className="table">
          <thead>
            <tr>
              <th className="w-[120px]">API Key</th>
              <th>模型</th>
              <th className="w-[60px]">类型</th>
              <th className="w-[180px]">Tokens</th>
              <th className="w-[120px]">费用</th>
              <th className="w-[80px]">耗时</th>
              <th className="w-[160px]">时间</th>
              <th className="w-[50px]">状态</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={8} className="h-40 text-center">
                  <div className="flex items-center justify-center gap-2 text-gray-400">
                    <RefreshCw className="w-4 h-4 animate-spin text-primary-500" />
                    加载中...
                  </div>
                </td>
              </tr>
            ) : logs.length === 0 ? (
              <tr>
                <td colSpan={8} className="h-40 text-center text-gray-400 dark:text-dark-500">
                  <div className="flex flex-col items-center gap-2">
                    <FileText className="w-10 h-10 opacity-30" />
                    <span>所选范围内暂无使用记录</span>
                  </div>
                </td>
              </tr>
            ) : logs.map(log => (
              <tr key={log.id} className="group">
                {/* API Key */}
                <td>
                  <span className="text-sm font-medium text-gray-900 dark:text-white truncate block max-w-[120px]" title={log.api_key_name}>
                    {log.api_key_name || '-'}
                  </span>
                </td>

                {/* Model */}
                <td>
                  <span className="font-mono text-xs font-medium text-gray-900 dark:text-white" title={log.model}>
                    {log.model}
                  </span>
                  {log.provider && (
                    <span className="ml-1.5 inline-flex items-center rounded-md px-1.5 py-0.5 text-[10px] font-medium bg-gray-100 dark:bg-dark-800 text-gray-500 dark:text-dark-400">
                      {log.provider}
                    </span>
                  )}
                </td>

                {/* Type */}
                <td>
                  <span className={`inline-flex items-center rounded-md px-2 py-0.5 text-[11px] font-semibold ${
                    log.stream
                      ? 'bg-blue-50 dark:bg-blue-900/20 text-blue-600 dark:text-blue-400'
                      : 'bg-gray-100 dark:bg-dark-800 text-gray-600 dark:text-gray-400'
                  }`}>
                    {log.stream ? 'Stream' : 'Sync'}
                  </span>
                </td>

                {/* Tokens */}
                <td>
                  <div className="flex items-center gap-1.5">
                    <div className="space-y-0.5">
                      <div className="flex items-center gap-2 text-sm">
                        <div className="inline-flex items-center gap-0.5">
                          <ArrowDownCircle className="w-3 h-3 text-emerald-500" />
                          <span className="font-medium text-gray-900 dark:text-white tabular-nums">{log.input_tokens.toLocaleString()}</span>
                        </div>
                        <div className="inline-flex items-center gap-0.5">
                          <ArrowUpCircle className="w-3 h-3 text-violet-500" />
                          <span className="font-medium text-gray-900 dark:text-white tabular-nums">{log.output_tokens.toLocaleString()}</span>
                        </div>
                      </div>
                      {(log.cached_tokens > 0 || log.reasoning_tokens > 0) && (
                        <div className="flex items-center gap-2 text-[11px]">
                          {log.cached_tokens > 0 && (
                            <span className="text-sky-500 tabular-nums">缓存 {fmtTokens(log.cached_tokens)}</span>
                          )}
                          {log.reasoning_tokens > 0 && (
                            <span className="text-amber-500 tabular-nums">推理 {fmtTokens(log.reasoning_tokens)}</span>
                          )}
                        </div>
                      )}
                    </div>
                    <div
                      className="flex h-4 w-4 cursor-help items-center justify-center rounded-full bg-gray-100 dark:bg-dark-800 transition-colors hover:bg-blue-100 dark:hover:bg-blue-900/30 opacity-0 group-hover:opacity-100"
                      onMouseEnter={(e) => onTokenTooltip({ log, x: e.clientX, y: e.clientY })}
                      onMouseLeave={() => onTokenTooltip(null)}
                    >
                      <Info className="w-2.5 h-2.5 text-gray-400 dark:text-dark-500" />
                    </div>
                  </div>
                </td>

                {/* Cost */}
                <td>
                  <div className="flex items-center gap-1.5">
                    <span className="font-medium text-green-600 dark:text-green-400 tabular-nums text-sm">
                      {fmtCost(log.actual_cost)}
                    </span>
                    <div
                      className="flex h-4 w-4 cursor-help items-center justify-center rounded-full bg-gray-100 dark:bg-dark-800 transition-colors hover:bg-blue-100 dark:hover:bg-blue-900/30 opacity-0 group-hover:opacity-100"
                      onMouseEnter={(e) => onCostTooltip({ log, x: e.clientX, y: e.clientY })}
                      onMouseLeave={() => onCostTooltip(null)}
                    >
                      <Info className="w-2.5 h-2.5 text-gray-400 dark:text-dark-500" />
                    </div>
                  </div>
                </td>

                {/* Duration */}
                <td>
                  <span className="text-sm text-gray-500 dark:text-gray-400 tabular-nums">{fmtDuration(log.duration_ms)}</span>
                </td>

                {/* Time */}
                <td>
                  <span className="text-sm text-gray-500 dark:text-gray-400 tabular-nums">{fmtDateTime(log.created_at)}</span>
                </td>

                {/* Status */}
                <td className="text-center">
                  {log.failed ? (
                    <XCircle className="w-4 h-4 text-red-500 inline-block" />
                  ) : (
                    <CheckCircle2 className="w-4 h-4 text-green-500 inline-block" />
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Pagination */}
      {total > 0 && (
        <div className="px-5 py-3 border-t border-border flex items-center justify-between bg-gray-50/50 dark:bg-dark-800/30">
          <div className="text-xs text-gray-500 dark:text-dark-400 tabular-nums">
            共 {total.toLocaleString()} 条记录 · 第 {page}/{totalPages} 页
          </div>
          <div className="flex items-center gap-1.5">
            {/* Page size selector */}
            <select
              value={pageSize}
              onChange={(e) => { onPageSizeChange(Number(e.target.value)); onPageChange(1) }}
              className="h-8 rounded-lg border border-border bg-white dark:bg-dark-900 px-2 text-xs text-gray-600 dark:text-gray-300 outline-none"
            >
              {[20, 50, 100].map(n => (
                <option key={n} value={n}>{n} 条/页</option>
              ))}
            </select>

            <button
              disabled={page <= 1}
              onClick={() => onPageChange(page - 1)}
              className="h-8 w-8 rounded-lg border border-border bg-white dark:bg-dark-900 flex items-center justify-center text-gray-500 hover:bg-gray-50 dark:hover:bg-dark-800 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
            >
              <ChevronLeft className="w-4 h-4" />
            </button>

            {/* Page number buttons */}
            {(() => {
              const pages: (number | '...')[] = []
              if (totalPages <= 7) {
                for (let i = 1; i <= totalPages; i++) pages.push(i)
              } else {
                pages.push(1)
                if (page > 3) pages.push('...')
                for (let i = Math.max(2, page - 1); i <= Math.min(totalPages - 1, page + 1); i++) pages.push(i)
                if (page < totalPages - 2) pages.push('...')
                pages.push(totalPages)
              }
              return pages.map((p, i) =>
                p === '...' ? (
                  <span key={`dots-${i}`} className="w-8 text-center text-xs text-gray-400">···</span>
                ) : (
                  <button
                    key={p}
                    onClick={() => onPageChange(p)}
                    className={`h-8 min-w-[32px] px-2 rounded-lg border text-xs font-medium transition-colors ${
                      p === page
                        ? 'border-primary-400 bg-primary-50 dark:bg-primary-900/20 text-primary-600 dark:text-primary-400'
                        : 'border-border bg-white dark:bg-dark-900 text-gray-600 dark:text-gray-400 hover:bg-gray-50 dark:hover:bg-dark-800'
                    }`}
                  >
                    {p}
                  </button>
                )
              )
            })()}

            <button
              disabled={page >= totalPages}
              onClick={() => onPageChange(page + 1)}
              className="h-8 w-8 rounded-lg border border-border bg-white dark:bg-dark-900 flex items-center justify-center text-gray-500 hover:bg-gray-50 dark:hover:bg-dark-800 disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
            >
              <ChevronRight className="w-4 h-4" />
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
