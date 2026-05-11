import type { SDKModelDefinition } from '@/features/pricing/model_prices'
import type { QuotaResult } from '@/features/usage/quota'

// ── AuthFile types ────────────────────────────────────────────────

export interface AuthFileItem {
  name: string
  provider?: string
  type?: string
  size?: string | number
  status?: string
  status_message?: string
  disabled?: boolean
  success?: number
  failed?: number
  recent_requests?: Array<{ success?: number; failed?: number; start?: string; end?: string }>
  email?: string
  label?: string
  updated_at?: string
  source_kind?: string
  auth_type?: string
  auth_index?: string | number
  authIndex?: string | number
  chatgpt_account_id?: string
  project_id?: string
  state?: string
  models?: string[]
  [key: string]: unknown
}

export type SmartView = 'all' | 'healthy' | 'warning' | 'disabled'

// ── Component prop types ────────────────────────────────────────

export interface AuthFileToolbarProps {
  smartView: SmartView
  onSmartViewChange: (v: SmartView) => void
  filterProvider: string
  onFilterProviderChange: (p: string) => void
  searchQuery: string
  onSearchChange: (q: string) => void
  providers: string[]
  counts: { all: number; healthy: number; warning: number; disabled: number }
  selectedCount: number
  selectedPausableCount: number
  selectedResumableCount: number
  batchStatusLoading: boolean
  onBatchPause: () => void
  onBatchResume: () => void
  onBatchDelete: () => void
  onRefresh: () => void
  refreshing: boolean
  onUpload: () => void
}

export interface AuthFileTableProps {
  files: AuthFileItem[]
  selectedNames: Set<string>
  allSelected: boolean
  indeterminate: boolean
  loading: boolean
  totalCount: number
  onSelectAll: (checked: boolean) => void
  onSelectOne: (name: string, checked: boolean) => void
  onToggle: (file: AuthFileItem) => void
  onDelete: (file: AuthFileItem) => void
  onDetail: (file: AuthFileItem) => void
  onLoadQuota: (file: AuthFileItem) => void
  quotaLoading: Record<string, boolean>
  providerBadgeColor: (p?: string) => string
  fmtRelative: (d?: string | null) => string
  stateInfo: (f: AuthFileItem) => { icon: React.ReactNode; label: string; tone: string }
}

export interface AuthFileDialogProps {
  open: boolean
  onOpenChange: (o: boolean) => void
  onConfirm: () => void
  loading: boolean
  item?: AuthFileItem | null
  title: string
  description: string
  confirmLabel: string
  children?: React.ReactNode
}

export interface AuthFileUploadDialogProps {
  open: boolean
  onOpenChange: (o: boolean) => void
  onUpload: () => void
  uploading: boolean
  value: string
  onValueChange: (v: string) => void
  /** 多文件上传（与粘贴二选一，由对话框写入） */
  pickedFiles: File[]
  onPickedFilesChange: (files: File[]) => void
}

export interface AuthFileDeleteDialogProps {
  open: boolean
  onOpenChange: (o: boolean) => void
  onConfirm: () => void
  deleting: boolean
  item: AuthFileItem | null
}

export interface AuthFileBatchDeleteDialogProps {
  open: boolean
  onOpenChange: (o: boolean) => void
  onConfirm: () => void
  deleting: boolean
  count: number
}

export interface AuthFileDetailDialogProps {
  open: boolean
  onOpenChange: (o: boolean) => void
  item: AuthFileItem | null
  onToggle: (file: AuthFileItem) => void
  onDelete: (file: AuthFileItem) => void
  quotaResults: Record<string, QuotaResult>
  quotaLoading: Record<string, boolean>
  onLoadQuota: (file: AuthFileItem) => void
  authModels: Record<string, SDKModelDefinition[]>
  authModelsLoading: Record<string, boolean>
  onLoadAuthModels: (file: AuthFileItem) => void
  fmtRelative: (d?: string | null) => string
  stateInfo: (f: AuthFileItem) => { icon: React.ReactNode; label: string; tone: string }
  providerBadgeColor: (p?: string) => string
}

export interface AuthFileQuotaPanelProps {
  item: AuthFileItem
  quota: QuotaResult | undefined
  loading: boolean
  onRefresh: () => void
}

export interface AuthFileModelsPanelProps {
  item: AuthFileItem
  models: SDKModelDefinition[] | undefined
  loading: boolean
  onRefresh: () => void
}
