import { useEffect, useState, useCallback, useMemo, useRef } from 'react'
import { fetchMgmtApi, fetchMgmtApiFormData } from '@/features/admin-proxy/api'
import { buildAuthFilesFormData } from '@/features/admin-proxy/authFileImportUtils'
import { getBatchStatusTargets, providerBadgeColor } from '@/features/admin-proxy/authFileViewUtils'
import { fetchQuotaForFile, type QuotaResult } from '@/features/usage/quota'
import { modelsApi } from '@/features/pricing/model_prices'
import { toast } from 'sonner'
import { ShieldCheck, ShieldX, ShieldAlert } from 'lucide-react'
import type { AuthFileItem, SmartView } from '@/features/admin-proxy/types'
import { AuthFileToolbar } from '@/features/admin-proxy/components/AuthFileToolbar'
import { AuthFileTable } from '@/features/admin-proxy/components/AuthFileTable'
import { AuthFileUploadDialog } from '@/features/admin-proxy/components/AuthFileUploadDialog'
import { AuthFileDeleteDialog } from '@/features/admin-proxy/components/AuthFileDeleteDialog'
import { AuthFileBatchDeleteDialog } from '@/features/admin-proxy/components/AuthFileBatchDeleteDialog'
import { AuthFileDetailDialog } from '@/features/admin-proxy/components/AuthFileDetailDialog'
import type { SDKModelDefinition } from '@/features/pricing/model_prices'

// ── Helpers ─────────────────────────────────────────────────────────────────

function isWarning(f: AuthFileItem) {
  return !f.disabled && (f.status === 'error' || f.status === 'offline' || f.state === 'cooling' || f.state === 'blocked' || !!f.status_message)
}

function isHealthy(f: AuthFileItem) {
  return !f.disabled && !isWarning(f)
}

function isDisabled(f: AuthFileItem) {
  return !!f.disabled
}

function fmtRelative(dateStr?: string | null) {
  if (!dateStr) return '—'
  try {
    const ts = new Date(dateStr).getTime()
    if (isNaN(ts)) return String(dateStr)
    const diffSec = Math.round((ts - Date.now()) / 1000)
    const abs = Math.abs(diffSec)
    const fmt = new Intl.RelativeTimeFormat('zh-CN', { numeric: 'auto' })
    if (abs < 60) return fmt.format(diffSec, 'second')
    if (abs < 3600) return fmt.format(Math.round(diffSec / 60), 'minute')
    if (abs < 86400) return fmt.format(Math.round(diffSec / 3600), 'hour')
    return fmt.format(Math.round(diffSec / 86400), 'day')
  } catch { return String(dateStr) }
}

function stateInfo(f: AuthFileItem) {
  if (f.disabled) return { icon: <ShieldX className="h-3.5 w-3.5" />, label: '已停用', tone: 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-400' }

  const msg = (f.status_message || '').toLowerCase()
  if (f.state === 'blocked' || f.status === 'error' || msg.includes('401') || msg.includes('失效') || msg.includes('error')) {
    return { icon: <ShieldX className="h-3.5 w-3.5" />, label: '异常', tone: 'bg-red-100 text-red-700 dark:bg-red-950/40 dark:text-red-300' }
  }

  if (f.state === 'cooling' || f.status === 'offline' || !!f.status_message) {
    return { icon: <ShieldAlert className="h-3.5 w-3.5" />, label: '异常', tone: 'bg-amber-100 text-amber-700 dark:bg-amber-950/40 dark:text-amber-300' }
  }

  return { icon: <ShieldCheck className="h-3.5 w-3.5" />, label: '正常', tone: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300' }
}

// ── Page Component ──────────────────────────────────────────────────────────

export default function AdminProxyAuthFilesPage() {
  const [authFiles, setAuthFiles] = useState<AuthFileItem[]>([])
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)

  // Filters
  const [searchQuery, setSearchQuery] = useState('')
  const [filterProvider, setFilterProvider] = useState('all')
  const [smartView, setSmartView] = useState<SmartView>('all')

  // Upload dialog
  const [showUpload, setShowUpload] = useState(false)
  const [uploadText, setUploadText] = useState('')
  const [uploadPickedFiles, setUploadPickedFiles] = useState<File[]>([])
  const [uploading, setUploading] = useState(false)

  // Delete dialog
  const [showDelete, setShowDelete] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<AuthFileItem | null>(null)
  const [deleting, setDeleting] = useState(false)

  // Detail dialog
  const [showDetail, setShowDetail] = useState(false)
  const [detailItem, setDetailItem] = useState<AuthFileItem | null>(null)

  // Selection
  const [selectedNames, setSelectedNames] = useState<Set<string>>(new Set())
  const [showBatchDelete, setShowBatchDelete] = useState(false)
  const [batchDeleting, setBatchDeleting] = useState(false)
  const [batchStatusLoading, setBatchStatusLoading] = useState(false)

  // Quota
  const quotaCache = useRef<Record<string, { result: QuotaResult; ts: number }>>({})
  const [quotaLoading, setQuotaLoading] = useState<Record<string, boolean>>({})
  const [quotaResults, setQuotaResults] = useState<Record<string, QuotaResult>>({})

  // Registered models
  const [authModels, setAuthModels] = useState<Record<string, SDKModelDefinition[]>>({})
  const [authModelsLoading, setAuthModelsLoading] = useState<Record<string, boolean>>({})

  // ── Fetch ──
  const fetchAll = useCallback(async (silent = false, notify = false) => {
    if (!silent) setLoading(true)
    else setRefreshing(true)
    try {
      const res = await fetchMgmtApi('/auth-files')
      const parsed = res?.files || res?.['auth-files'] || res?.authFiles || []
      setAuthFiles(Array.isArray(parsed) ? parsed : [])
      if (notify) toast.success('已同步 SDK 凭证列表')
    } catch (e: unknown) {
      if (!silent || notify) {
        const prefix = notify ? '刷新列表失败' : '读取失败'
        toast.error(`${prefix}: ${e instanceof Error ? e.message : String(e)}`)
      }
    } finally {
      setLoading(false)
      setRefreshing(false)
    }
  }, [])

  useEffect(() => {
    const timer = globalThis.setTimeout(() => { void fetchAll() }, 0)
    return () => globalThis.clearTimeout(timer)
  }, [fetchAll])

  // ── Computed ──
  const providers = useMemo(() => {
    const set = new Set(authFiles.map(f => f.provider || 'unknown').filter(Boolean))
    return Array.from(set).sort()
  }, [authFiles])

  const counts = useMemo(() => ({
    all: authFiles.length,
    healthy: authFiles.filter(isHealthy).length,
    warning: authFiles.filter(isWarning).length,
    disabled: authFiles.filter(isDisabled).length,
  }), [authFiles])

  const processedFiles = useMemo(() => {
    let items = [...authFiles]

    if (smartView === 'healthy') items = items.filter(isHealthy)
    else if (smartView === 'warning') items = items.filter(isWarning)
    else if (smartView === 'disabled') items = items.filter(isDisabled)

    if (filterProvider !== 'all') {
      items = items.filter(f => (f.provider || 'unknown') === filterProvider)
    }

    if (searchQuery.trim()) {
      const q = searchQuery.toLowerCase()
      items = items.filter(f =>
        f.name?.toLowerCase().includes(q) ||
        f.email?.toLowerCase().includes(q) ||
        f.label?.toLowerCase().includes(q) ||
        f.provider?.toLowerCase().includes(q)
      )
    }

    return items
  }, [authFiles, smartView, filterProvider, searchQuery])

  const selectedVisibleCount = processedFiles.filter(f => selectedNames.has(f.name)).length
  const allSelected = processedFiles.length > 0 && selectedVisibleCount === processedFiles.length
  const indeterminate = selectedVisibleCount > 0 && selectedVisibleCount < processedFiles.length
  const batchStatusTargets = useMemo(
    () => getBatchStatusTargets(authFiles, selectedNames),
    [authFiles, selectedNames]
  )

  // ── Actions ──
  const handleSelectAll = (checked: boolean) => {
    if (checked) {
      setSelectedNames(new Set(processedFiles.map(f => f.name)))
    } else {
      setSelectedNames(new Set())
    }
  }

  const handleSelectOne = (name: string, checked: boolean) => {
    setSelectedNames(prev => {
      const next = new Set(prev)
      if (checked) next.add(name)
      else next.delete(name)
      return next
    })
  }

  const handleToggle = async (file: AuthFileItem) => {
    const nextDisabled = !file.disabled
    try {
      await fetchMgmtApi('/auth-files/status', {
        method: 'PATCH',
        body: JSON.stringify({ name: file.name, disabled: nextDisabled }),
      })
      toast.success(nextDisabled ? '已暂停调度' : '已恢复调度')
      void fetchAll(true)
    } catch (e: unknown) {
      toast.error(`操作失败: ${e instanceof Error ? e.message : String(e)}`)
    }
  }

  const handleRefreshList = useCallback(() => {
    void fetchAll(true, true)
  }, [fetchAll])

  const openDelete = (file: AuthFileItem) => {
    setDeleteTarget(file)
    setShowDelete(true)
  }

  const handleDelete = async () => {
    if (!deleteTarget) return
    setDeleting(true)
    try {
      await fetchMgmtApi(`/auth-files?name=${encodeURIComponent(deleteTarget.name)}`, { method: 'DELETE' })
      toast.success('凭证已删除')
      setShowDelete(false)
      setSelectedNames(prev => {
        const next = new Set(prev)
        next.delete(deleteTarget.name)
        return next
      })
      void fetchAll(true)
    } catch (e: unknown) {
      toast.error(`删除失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setDeleting(false)
    }
  }

  const handleBatchStatus = async (disabled: boolean) => {
    const targets = disabled ? batchStatusTargets.pauseTargets : batchStatusTargets.resumeTargets
    if (targets.length === 0) return

    setBatchStatusLoading(true)
    let successCount = 0
    let failCount = 0

    try {
      for (const file of targets) {
        try {
          await fetchMgmtApi('/auth-files/status', {
            method: 'PATCH',
            body: JSON.stringify({ name: file.name, disabled }),
          })
          successCount++
        } catch (e) {
          console.error(`Failed to update ${file.name}`, e)
          failCount++
        }
      }

      if (failCount === 0) {
        toast.success(disabled ? `已暂停 ${successCount} 个凭证` : `已恢复 ${successCount} 个凭证`)
      } else {
        toast.warning(`${disabled ? '暂停' : '恢复'}完成: ${successCount} 成功, ${failCount} 失败`)
      }
      setSelectedNames(new Set())
      await fetchAll(true)
    } finally {
      setBatchStatusLoading(false)
    }
  }

  const handleBatchDelete = async () => {
    if (selectedNames.size === 0) return
    setBatchDeleting(true)
    let successCount = 0
    let failCount = 0

    try {
      const names = Array.from(selectedNames)
      for (const name of names) {
        try {
          await fetchMgmtApi(`/auth-files?name=${encodeURIComponent(name)}`, { method: 'DELETE' })
          successCount++
        } catch (e) {
          console.error(`Failed to delete ${name}`, e)
          failCount++
        }
      }

      if (failCount === 0) {
        toast.success(`成功删除 ${successCount} 个凭证`)
      } else {
        toast.warning(`删除完成: ${successCount} 成功, ${failCount} 失败`)
      }
      setSelectedNames(new Set())
      setShowBatchDelete(false)
      void fetchAll(true)
    } finally {
      setBatchDeleting(false)
    }
  }

  const handleUpload = async () => {
    const text = uploadText.trim()
    const hasFiles = uploadPickedFiles.length > 0
    if (!hasFiles && !text) {
      toast.error('请粘贴凭证内容或选择文件')
      return
    }
    setUploading(true)
    try {
      if (hasFiles) {
        const form = buildAuthFilesFormData(uploadPickedFiles)
        const res = (await fetchMgmtApiFormData('/auth-files', form)) as Record<string, unknown> | string
        if (res && typeof res === 'object' && res.status === 'partial' && Array.isArray(res.failed) && res.failed.length) {
          const uploaded = typeof res.uploaded === 'number' ? res.uploaded : 0
          toast.warning(`部分成功：已上传 ${uploaded} 个，${res.failed.length} 个失败`)
        } else if (res && typeof res === 'object' && typeof res.uploaded === 'number' && res.uploaded > 1) {
          toast.success(`凭证导入成功（${res.uploaded} 个文件）`)
        } else {
          toast.success('凭证导入成功')
        }
      } else {
        const form = new FormData()
        form.append('file', new File([text], 'pasted-import.json', { type: 'application/json' }))
        await fetchMgmtApiFormData('/auth-files', form)
        toast.success('凭证导入成功')
      }
      setShowUpload(false)
      setUploadText('')
      setUploadPickedFiles([])
      void fetchAll(true)
    } catch (e: unknown) {
      toast.error(`导入失败: ${e instanceof Error ? e.message : String(e)}`)
    } finally {
      setUploading(false)
    }
  }

  // ── Quota & Models ──
  const loadQuota = async (file: AuthFileItem) => {
    const cached = quotaCache.current[file.name]
    if (cached && Date.now() - cached.ts < 60_000) {
      setQuotaResults(prev => ({ ...prev, [file.name]: cached.result }))
      return
    }
    setQuotaLoading(prev => ({ ...prev, [file.name]: true }))
    try {
      const result = await fetchQuotaForFile(file)
      quotaCache.current[file.name] = { result, ts: Date.now() }
      setQuotaResults(prev => ({ ...prev, [file.name]: result }))
    } catch (e: unknown) {
      const err = e instanceof Error ? e.message : '查询失败'
      setQuotaResults(prev => ({ ...prev, [file.name]: { provider: file.provider || '', windows: [], error: err } }))
    } finally {
      setQuotaLoading(prev => ({ ...prev, [file.name]: false }))
    }
  }

  const loadAuthModels = async (file: AuthFileItem) => {
    const key = file.name
    if (!key) return
    setAuthModelsLoading(prev => ({ ...prev, [key]: true }))
    try {
      const models = await modelsApi.fetchAuthFileModels(key)
      setAuthModels(prev => ({ ...prev, [key]: models }))
    } catch (e: unknown) {
      const err = e instanceof Error ? e.message : '查询失败'
      toast.error(`获取已注册模型失败: ${err}`)
      setAuthModels(prev => ({ ...prev, [key]: [] }))
    } finally {
      setAuthModelsLoading(prev => ({ ...prev, [key]: false }))
    }
  }

  const openDetail = (file: AuthFileItem) => {
    setDetailItem(file)
    setShowDetail(true)
    if ((file.auth_index ?? file.authIndex) !== undefined) {
      loadQuota(file)
    }
    if (!authModels[file.name]) {
      loadAuthModels(file)
    }
  }

  return (
    <div className="space-y-4">
      <AuthFileToolbar
        smartView={smartView}
        onSmartViewChange={setSmartView}
        filterProvider={filterProvider}
        onFilterProviderChange={setFilterProvider}
        searchQuery={searchQuery}
        onSearchChange={setSearchQuery}
        providers={providers}
        counts={counts}
        selectedCount={selectedNames.size}
        selectedPausableCount={batchStatusTargets.pauseTargets.length}
        selectedResumableCount={batchStatusTargets.resumeTargets.length}
        batchStatusLoading={batchStatusLoading}
        onBatchPause={() => { void handleBatchStatus(true) }}
        onBatchResume={() => { void handleBatchStatus(false) }}
        onBatchDelete={() => setShowBatchDelete(true)}
        onRefresh={handleRefreshList}
        refreshing={refreshing}
        onUpload={() => {
          setUploadText('')
          setUploadPickedFiles([])
          setShowUpload(true)
        }}
      />

      <AuthFileTable
        files={processedFiles}
        selectedNames={selectedNames}
        allSelected={allSelected}
        indeterminate={indeterminate}
        loading={loading}
        totalCount={authFiles.length}
        onSelectAll={handleSelectAll}
        onSelectOne={handleSelectOne}
        onToggle={handleToggle}
        onDelete={openDelete}
        onDetail={openDetail}
        onLoadQuota={loadQuota}
        quotaLoading={quotaLoading}
        providerBadgeColor={providerBadgeColor}
        fmtRelative={fmtRelative}
        stateInfo={stateInfo}
      />

      <AuthFileUploadDialog
        key={showUpload ? 'upload-open' : 'upload-closed'}
        open={showUpload}
        onOpenChange={o => {
          setShowUpload(o)
          if (!o) {
            setUploadText('')
            setUploadPickedFiles([])
          }
        }}
        onUpload={handleUpload}
        uploading={uploading}
        value={uploadText}
        onValueChange={setUploadText}
        pickedFiles={uploadPickedFiles}
        onPickedFilesChange={setUploadPickedFiles}
      />

      <AuthFileBatchDeleteDialog
        open={showBatchDelete}
        onOpenChange={setShowBatchDelete}
        onConfirm={handleBatchDelete}
        deleting={batchDeleting}
        count={selectedNames.size}
      />

      <AuthFileDeleteDialog
        open={showDelete}
        onOpenChange={setShowDelete}
        onConfirm={handleDelete}
        deleting={deleting}
        item={deleteTarget}
      />

      <AuthFileDetailDialog
        open={showDetail}
        onOpenChange={setShowDetail}
        item={detailItem}
        onToggle={handleToggle}
        onDelete={openDelete}
        quotaResults={quotaResults}
        quotaLoading={quotaLoading}
        onLoadQuota={loadQuota}
        authModels={authModels}
        authModelsLoading={authModelsLoading}
        onLoadAuthModels={loadAuthModels}
        fmtRelative={fmtRelative}
        stateInfo={stateInfo}
        providerBadgeColor={providerBadgeColor}
      />
    </div>
  )
}
