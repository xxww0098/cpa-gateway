import { useState, useEffect, useCallback, useRef, memo } from "react"
import { useParams, useNavigate } from "react-router-dom"
import { fetchApi } from "@/shared/api/client"
import { DEFAULT_TICKET_QUICK_REPLIES } from "@/shared/constants/ticketQuickReplyDefaults"
import { toast } from "sonner"
import { Button } from "@/shared/components/ui/button"
import { Textarea } from "@/shared/components/ui/textarea"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/shared/components/ui/select"
import { ChevronLeft, ChevronRight, Send, Loader2, User, Shield } from "lucide-react"
import { TicketRichContent } from "@/features/tickets/TicketRichContent"

interface TicketItem {
  id: number
  title: string
  category: string
  status: string
  priority: string
  user_id?: number
  user_email?: string
  assigned_to?: number | null
  created_at: string
  updated_at: string
}

interface TicketReply {
  id: number
  user_id: number
  content: string
  is_staff: boolean
  created_at: string
}

interface TicketDetail {
  id: number
  title: string
  category: string
  status: string
  priority: string
  user_id: number
  user_email?: string
  assigned_to?: number | null
  created_at: string
  updated_at: string
  closed_at?: string | null
  replies: TicketReply[]
}

const STATUS_LABEL: Record<string, string> = {
  open: "待处理",
  pending: "处理中",
  resolved: "已解决",
  closed: "已关闭",
}

const CATEGORY_MAP: Record<string, string> = {
  payment: "支付问题",
  technical: "技术问题",
  account: "账户问题",
  other: "其他",
}

const PRIORITY_LABEL: Record<string, string> = {
  low: "低",
  medium: "中",
  high: "高",
  urgent: "紧急",
}

const STATUS_OPTIONS = [
  { key: "open", label: "待处理" },
  { key: "pending", label: "处理中" },
  { key: "resolved", label: "已解决" },
  { key: "closed", label: "已关闭" },
]

function fmtDateTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function fmtShortTime(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, "0")
  return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}

function userLabel(t: TicketItem): string {
  if (t.user_email) return t.user_email
  if (typeof t.user_id === "number") return `用户#${t.user_id}`
  return "未知用户"
}

function userInitial(label: string): string {
  const c = label.trim().charAt(0)
  return c ? c.toUpperCase() : "?"
}

const FILTER_KEYS = [
  { key: "", label: "全部" },
  { key: "open", label: "待处理" },
  { key: "pending", label: "处理中" },
  { key: "resolved", label: "已解决" },
  { key: "closed", label: "已关闭" },
] as const

type TicketChatPaneProps = {
  ticketId: string | undefined
  onTicketUpdated: () => void
}

const TicketChatPane = memo(function TicketChatPane({ ticketId, onTicketUpdated }: TicketChatPaneProps) {
  const [ticket, setTicket] = useState<TicketDetail | null>(null)
  const [loading, setLoading] = useState(false)
  const [replyContent, setReplyContent] = useState("")
  const [sending, setSending] = useState(false)
  const [updatingStatus, setUpdatingStatus] = useState(false)
  const [assigning, setAssigning] = useState(false)
  const [quickReplies, setQuickReplies] = useState(() => [...DEFAULT_TICKET_QUICK_REPLIES])
  const messagesEndRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetchApi("/admin/ticket-quick-replies")
        const list = res?.data?.items as { label: string; text: string }[] | undefined
        if (!cancelled && Array.isArray(list) && list.length > 0) {
          setQuickReplies(list.map((r) => ({ label: String(r.label ?? ""), text: String(r.text ?? "") })))
        }
      } catch {
        if (!cancelled) setQuickReplies([...DEFAULT_TICKET_QUICK_REPLIES])
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const loadTicket = useCallback(
    async (opts?: { silent?: boolean }) => {
      if (!ticketId) return
      const silent = opts?.silent ?? false
      if (!silent) setLoading(true)
      try {
        const res = await fetchApi(`/admin/tickets/${ticketId}`)
        if (res?.data) {
          setTicket(res.data)
        } else {
          setTicket(null)
        }
      } catch {
        setTicket(null)
      } finally {
        if (!silent) setLoading(false)
      }
    },
    [ticketId]
  )

  useEffect(() => {
    if (!ticketId) {
      setTicket(null)
      return
    }
    void loadTicket({ silent: false })
  }, [ticketId, loadTicket])

  useEffect(() => {
    messagesEndRef.current?.scrollIntoView({ behavior: "auto", block: "end" })
  }, [ticket?.replies?.length, ticketId])

  const submitReply = useCallback(
    async (raw: string) => {
      const content = raw.trim()
      if (!ticketId || !content) return
      setSending(true)
      try {
        await fetchApi(`/admin/tickets/${ticketId}/replies`, {
          method: "POST",
          body: JSON.stringify({ content }),
        })
        setReplyContent("")
        toast.success("回复成功")
        await loadTicket({ silent: true })
        onTicketUpdated()
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "回复失败")
      } finally {
        setSending(false)
      }
    },
    [ticketId, loadTicket, onTicketUpdated]
  )

  const handleReply = () => void submitReply(replyContent)

  const handleStatusChange = async (newStatus: string) => {
    if (!ticketId || newStatus === ticket?.status) return
    setUpdatingStatus(true)
    try {
      await fetchApi(`/admin/tickets/${ticketId}/status`, {
        method: "PUT",
        body: JSON.stringify({ status: newStatus }),
      })
      toast.success("状态已更新")
      await loadTicket({ silent: true })
      onTicketUpdated()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "更新状态失败")
    } finally {
      setUpdatingStatus(false)
    }
  }

  const handleAssign = async (staffId: string) => {
    if (!ticketId) return
    setAssigning(true)
    try {
      await fetchApi(`/admin/tickets/${ticketId}/assign`, {
        method: "PUT",
        body: JSON.stringify({ staff_id: Number(staffId) }),
      })
      toast.success("工单已分配")
      await loadTicket({ silent: true })
      onTicketUpdated()
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "分配失败")
    } finally {
      setAssigning(false)
    }
  }

  if (!ticketId) {
    return (
      <div className="flex flex-1 items-center justify-center px-4 py-12 text-sm text-gray-500 dark:text-dark-400">
        在左侧选择一条工单
      </div>
    )
  }

  if (loading) {
    return (
      <div className="flex flex-1 items-center justify-center text-sm text-gray-500 dark:text-dark-400">
        加载中
      </div>
    )
  }

  if (!ticket) {
    return (
      <div className="flex flex-1 items-center justify-center px-4 text-sm text-gray-500 dark:text-dark-400">
        无法加载该工单
      </div>
    )
  }

  const statusLabel = STATUS_LABEL[ticket.status] ?? ticket.status
  const priorityLabel = PRIORITY_LABEL[ticket.priority] ?? ticket.priority
  const isClosed = ticket.status === "closed"
  const peerLabel = ticket.user_email || `用户#${ticket.user_id}`

  return (
    <div className="flex min-h-0 flex-1 flex-col bg-white dark:bg-dark-900">
      <header className="shrink-0 border-b border-border px-4 py-2.5">
        <h3 className="truncate text-sm font-medium text-gray-900 dark:text-white">{ticket.title}</h3>
        <p className="mt-0.5 text-xs text-gray-500 dark:text-dark-400">
          #{ticket.id} · {CATEGORY_MAP[ticket.category] || ticket.category} · {statusLabel} · 优先级 {priorityLabel} ·{" "}
          {peerLabel}
        </p>
        <div className="mt-2 flex flex-wrap gap-2">
          <button
            type="button"
            onClick={() => void loadTicket({ silent: true })}
            className="text-xs text-gray-500 underline-offset-2 hover:underline dark:text-dark-400"
          >
            刷新对话
          </button>
        </div>
      </header>

      <div className="flex min-h-0 flex-1 flex-col lg:flex-row">
        <div className="flex min-h-0 min-w-0 flex-1 flex-col border-b border-border lg:border-b-0 lg:border-r">
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-3">
            {ticket.replies.length === 0 ? (
              <p className="py-8 text-center text-xs text-gray-400 dark:text-dark-500">暂无回复</p>
            ) : (
              ticket.replies.map((reply) => {
                const isStaff = reply.is_staff
                return (
                  <div key={reply.id} className={isStaff ? "flex justify-end" : "flex justify-start"}>
                    <div
                      className={`flex max-w-[min(100%,480px)] gap-2 ${isStaff ? "flex-row-reverse" : "flex-row"}`}
                    >
                      <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-gray-100 text-gray-500 dark:bg-dark-800 dark:text-gray-400">
                        {isStaff ? <Shield className="h-3.5 w-3.5" /> : <User className="h-3.5 w-3.5" />}
                      </div>
                      <div
                        className={`min-w-0 rounded-lg px-3 py-2 text-sm ${
                          isStaff
                            ? "bg-primary-500/15 text-gray-900 dark:bg-primary-500/20 dark:text-gray-100"
                            : "bg-gray-100 text-gray-900 dark:bg-dark-800 dark:text-gray-100"
                        }`}
                      >
                        <div
                          className={`mb-1 text-[11px] text-gray-500 dark:text-dark-400 ${isStaff ? "text-right" : ""}`}
                        >
                          {isStaff ? "客服" : "用户"} · {fmtDateTime(reply.created_at)}
                        </div>
                        <TicketRichContent
                          content={reply.content}
                          className={isStaff ? "text-right" : ""}
                        />
                      </div>
                    </div>
                  </div>
                )
              })
            )}
            <div ref={messagesEndRef} />
          </div>

          {!isClosed && (
            <div className="shrink-0 border-t border-border p-3">
              <div className="mb-2 flex flex-wrap gap-1.5">
                {quickReplies.map((q, qi) => (
                  <button
                    key={`${qi}-${q.label}`}
                    type="button"
                    title={q.text}
                    disabled={sending || !q.text.trim()}
                    onClick={() => void submitReply(q.text)}
                    className="rounded-md border border-border bg-gray-50 px-2 py-1 text-xs text-gray-700 hover:bg-gray-100 disabled:opacity-50 dark:bg-dark-800 dark:text-gray-200 dark:hover:bg-dark-700"
                  >
                    {q.label || `回复${qi + 1}`}
                  </button>
                ))}
              </div>
              <div className="flex gap-2">
                <Textarea
                  placeholder="回复…"
                  value={replyContent}
                  onChange={(e) => setReplyContent(e.target.value)}
                  rows={2}
                  className="min-h-0 flex-1 resize-none text-sm"
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                      e.preventDefault()
                      void submitReply(replyContent)
                    }
                  }}
                />
                <Button onClick={handleReply} disabled={sending || !replyContent.trim()} size="sm" className="shrink-0 self-end">
                  {sending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
                </Button>
              </div>
            </div>
          )}

          {isClosed && (
            <div className="shrink-0 border-t border-border py-2 text-center text-xs text-gray-500 dark:text-dark-400">
              已关闭
            </div>
          )}
        </div>

        <aside className="w-full shrink-0 border-t border-border p-3 lg:w-[220px] lg:border-l lg:border-t-0">
          <div className="space-y-3">
            <div className="space-y-1">
              <label className="text-xs text-gray-500 dark:text-dark-400">状态</label>
              <Select value={ticket.status} onValueChange={(v) => void handleStatusChange(v)} disabled={updatingStatus}>
                <SelectTrigger className="h-9 text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {STATUS_OPTIONS.map((s) => (
                    <SelectItem key={s.key} value={s.key}>
                      {s.label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1">
              <label className="text-xs text-gray-500 dark:text-dark-400">分配</label>
              <Select
                value={ticket.assigned_to != null && ticket.assigned_to > 0 ? String(ticket.assigned_to) : "0"}
                onValueChange={(v) => void handleAssign(v)}
                disabled={assigning}
              >
                <SelectTrigger className="h-9 text-sm">
                  <SelectValue placeholder="选择" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="0">未分配</SelectItem>
                  <SelectItem value="1">客服#1</SelectItem>
                  <SelectItem value="2">客服#2</SelectItem>
                  <SelectItem value="3">客服#3</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="text-[11px] leading-relaxed text-gray-400 dark:text-dark-500">
              <div>创建 {fmtDateTime(ticket.created_at)}</div>
              <div>更新 {fmtDateTime(ticket.updated_at)}</div>
            </div>
          </div>
        </aside>
      </div>
    </div>
  )
})

export default function AdminTickets() {
  const { id: routeTicketId } = useParams<{ id?: string }>()
  const navigate = useNavigate()
  const [tickets, setTickets] = useState<TicketItem[]>([])
  const [listBusy, setListBusy] = useState(false)
  const [page, setPage] = useState(1)
  const [pageSize] = useState(20)
  const [total, setTotal] = useState(0)
  const [filterStatus, setFilterStatus] = useState("")
  const [listReady, setListReady] = useState(false)
  const listSoftRef = useRef(false)

  const loadList = useCallback(async () => {
    if (listSoftRef.current) setListBusy(true)
    try {
      const params = new URLSearchParams()
      params.set("page", String(page))
      params.set("page_size", String(pageSize))
      if (filterStatus) params.set("status", filterStatus)

      const listRes = await fetchApi(`/admin/tickets?${params}`)
      if (listRes?.data) {
        setTickets(listRes.data.items || [])
        setTotal(listRes.data.total || 0)
      }
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "加载工单列表失败")
    } finally {
      setListBusy(false)
      listSoftRef.current = true
      setListReady(true)
    }
  }, [page, pageSize, filterStatus])

  useEffect(() => {
    void loadList()
  }, [loadList])

  const handleFilter = useCallback((status: string) => {
    setFilterStatus(status)
    setPage(1)
  }, [])

  const onTicketUpdated = useCallback(() => {
    void loadList()
  }, [loadList])

  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const selectedFromList = tickets.find((t) => String(t.id) === routeTicketId)

  return (
    <div className="flex flex-col gap-3">
      <h2 className="text-lg font-semibold text-gray-900 dark:text-white">工单管理</h2>

      <div className="flex min-h-[min(100dvh-10rem,720px)] flex-col overflow-hidden rounded-lg border border-border bg-white dark:bg-dark-900 lg:flex-row">
        <aside className="flex w-full flex-col border-border lg:w-[min(100%,260px)] lg:max-w-[260px] lg:shrink-0 lg:border-r lg:bg-gray-50/50 dark:lg:bg-dark-950/50">
          {routeTicketId && !selectedFromList && (
            <div className="shrink-0 border-b border-border px-3 py-2 text-xs text-gray-500 dark:text-dark-400">
              工单 #{routeTicketId} 不在本页列表，可在右侧处理。
            </div>
          )}

          <div className="flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-2 py-1.5">
            {FILTER_KEYS.map((f) => (
              <button
                key={f.key || "all"}
                type="button"
                onClick={() => handleFilter(f.key)}
                className={`rounded px-2 py-1 text-xs ${
                  filterStatus === f.key
                    ? "bg-gray-200 text-gray-900 dark:bg-dark-700 dark:text-white"
                    : "text-gray-600 hover:bg-gray-100 dark:text-gray-400 dark:hover:bg-dark-800"
                }`}
              >
                {f.label}
              </button>
            ))}
            <button
              type="button"
              onClick={() => void loadList()}
              disabled={listBusy}
              className="ml-auto px-2 py-1 text-xs text-gray-500 hover:text-gray-700 disabled:opacity-50 dark:text-dark-400 dark:hover:text-gray-200"
            >
              {listBusy ? "更新中" : "刷新"}
            </button>
          </div>

          <div
            className={`min-h-[160px] flex-1 overflow-y-auto ${listBusy && tickets.length > 0 ? "opacity-70" : ""}`}
          >
            {!listReady && tickets.length === 0 ? (
              <div className="px-3 py-8 text-center text-xs text-gray-400 dark:text-dark-500">加载中</div>
            ) : tickets.length === 0 ? (
              <div className="px-3 py-8 text-center text-xs text-gray-400 dark:text-dark-500">暂无工单</div>
            ) : (
              <ul>
                {tickets.map((ticket) => {
                  const label = userLabel(ticket)
                  const active = String(ticket.id) === routeTicketId
                  const st = STATUS_LABEL[ticket.status] ?? ticket.status
                  const cat = CATEGORY_MAP[ticket.category] || ticket.category
                  return (
                    <li key={ticket.id} className="border-b border-border last:border-b-0">
                      <button
                        type="button"
                        onClick={() => navigate(`/admin/tickets/${ticket.id}`)}
                        className={`flex w-full gap-2 px-3 py-2.5 text-left ${
                          active ? "bg-gray-100 dark:bg-dark-800" : "hover:bg-gray-50 dark:hover:bg-dark-800/80"
                        }`}
                      >
                        <span className="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-gray-200 text-xs font-medium text-gray-600 dark:bg-dark-700 dark:text-gray-300">
                          {userInitial(label)}
                        </span>
                        <span className="min-w-0 flex-1">
                          <span className="flex items-baseline justify-between gap-2">
                            <span className="truncate text-sm text-gray-900 dark:text-gray-100">{label}</span>
                            <span className="shrink-0 tabular-nums text-[10px] text-gray-400 dark:text-dark-500">
                              {fmtShortTime(ticket.updated_at)}
                            </span>
                          </span>
                          <span className="mt-0.5 line-clamp-1 block text-xs text-gray-500 dark:text-dark-400">{ticket.title}</span>
                          <span className="mt-0.5 block text-[10px] text-gray-400 dark:text-dark-500">
                            {st} · {cat}
                          </span>
                        </span>
                      </button>
                    </li>
                  )
                })}
              </ul>
            )}
          </div>

          {total > 0 && (
            <div className="flex shrink-0 items-center justify-between border-t border-border px-2 py-1.5 text-[11px] text-gray-500 dark:text-dark-400">
              <span className="tabular-nums">
                {total} 条 · {page}/{totalPages}
              </span>
              <span className="flex gap-1">
                <button
                  type="button"
                  disabled={page <= 1 || listBusy}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                  className="rounded p-1 text-gray-500 hover:bg-gray-100 disabled:opacity-30 dark:hover:bg-dark-800"
                  aria-label="上一页"
                >
                  <ChevronLeft className="h-4 w-4" />
                </button>
                <button
                  type="button"
                  disabled={page >= totalPages || listBusy}
                  onClick={() => setPage((p) => p + 1)}
                  className="rounded p-1 text-gray-500 hover:bg-gray-100 disabled:opacity-30 dark:hover:bg-dark-800"
                  aria-label="下一页"
                >
                  <ChevronRight className="h-4 w-4" />
                </button>
              </span>
            </div>
          )}
        </aside>

        <div className="flex min-h-0 min-w-0 flex-1 flex-col">
          <TicketChatPane ticketId={routeTicketId} onTicketUpdated={onTicketUpdated} />
        </div>
      </div>
    </div>
  )
}
