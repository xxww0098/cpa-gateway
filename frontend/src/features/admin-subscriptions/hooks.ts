import { useState, useEffect, useCallback } from "react"
import { fetchApi } from "@/shared/api/client"
import { toast } from "sonner"
import type { Subscription, Group } from "./types"

interface UseSubscriptionsOptions {
  pageSize?: number
}

export function useSubscriptions(opts: UseSubscriptionsOptions = {}) {
  const { pageSize = 20 } = opts
  const [subs, setSubs] = useState<Subscription[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  const [loading, setLoading] = useState(true)
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)

  const loadData = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      const [subsRes, groupsRes] = await Promise.all([
        fetchApi(`/admin/subscriptions?page=${page}&page_size=${pageSize}`),
        fetchApi(`/admin/groups`),
      ])
      setSubs(subsRes.data.items || [])
      setTotal(subsRes.data.total || 0)
      setGroups((groupsRes.data || []).filter((g: Group) => g.subscription_type === "subscription"))
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "加载失败")
    } finally {
      setLoading(false)
    }
  }, [page, pageSize])

  useEffect(() => { loadData() }, [loadData])

  const handleExtend = async (id: number, days: number) => {
    try {
      await fetchApi(`/admin/subscriptions/${id}/extend`, {
        method: "PUT",
        body: JSON.stringify({ days }),
      })
      toast.success("续期成功")
      loadData(true)
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "续期失败")
    }
  }

  const handleRevoke = async (id: number) => {
    try {
      await fetchApi(`/admin/subscriptions/${id}`, { method: "DELETE" })
      toast.success("订阅已撤销")
      loadData(true)
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "撤销失败")
    }
  }

  const handleReactivate = async (id: number) => {
    const path = `/admin/subscriptions/${id}/reactivate`
    const isNotFoundMsg = (m: string) => /^not\s*found$/i.test(m.trim())
    try {
      await fetchApi(path, { method: "POST" })
      toast.success("订阅已恢复")
      loadData(true)
    } catch (errPost: unknown) {
      const msgPost = errPost instanceof Error ? errPost.message : ""
      if (isNotFoundMsg(msgPost)) {
        try {
          await fetchApi(path, { method: "PUT" })
          toast.success("订阅已恢复")
          loadData(true)
          return
        } catch (errPut: unknown) {
          const msgPut = errPut instanceof Error ? errPut.message : ""
          if (isNotFoundMsg(msgPut)) {
            toast.error("恢复接口未注册：请重新编译并重启 Go 后端（需含 /subscriptions/:id/reactivate）。")
            return
          }
          toast.error(msgPut || "恢复失败")
          return
        }
      }
      toast.error(msgPost || "恢复失败")
    }
  }

  const handleResetQuota = async (id: number) => {
    try {
      await fetchApi(`/admin/subscriptions/${id}/reset-quota`, { method: "POST" })
      toast.success("配额已重置")
      loadData(true)
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "重置失败")
    }
  }

  const totalPages = Math.ceil(total / pageSize)

  return {
    subs,
    groups,
    loading,
    total,
    page,
    setPage,
    totalPages,
    reload: () => loadData(false),
    loadData,
    handleExtend,
    handleRevoke,
    handleReactivate,
    handleResetQuota,
  }
}

export function useGroupCrud(onSaved?: () => void | Promise<void>) {
  const [groupDialogOpen, setGroupDialogOpen] = useState(false)
  const [editingGroupId, setEditingGroupId] = useState<number | null>(null)
  const [groupForm, setGroupForm] = useState({
    name: "",
    rate_multiplier: "1.0",
    daily_limit_usd: "",
    weekly_limit_usd: "",
    monthly_limit_usd: "",
    default_validity_days: "30",
    subscription_price_usd: "",
  })
  const [savingGroup, setSavingGroup] = useState(false)

  const openCreateGroup = () => {
    setEditingGroupId(null)
    setGroupForm({
      name: "",
      rate_multiplier: "1.0",
      daily_limit_usd: "",
      weekly_limit_usd: "",
      monthly_limit_usd: "",
      default_validity_days: "30",
      subscription_price_usd: "",
    })
    setGroupDialogOpen(true)
  }

  const openEditGroup = (g: Group) => {
    setEditingGroupId(g.id)
    setGroupForm({
      name: g.name,
      rate_multiplier: String(g.rate_multiplier ?? 1.0),
      daily_limit_usd: g.daily_limit_usd != null ? String(g.daily_limit_usd) : "",
      weekly_limit_usd: g.weekly_limit_usd != null ? String(g.weekly_limit_usd) : "",
      monthly_limit_usd: g.monthly_limit_usd != null ? String(g.monthly_limit_usd) : "",
      default_validity_days: String(g.default_validity_days ?? 30),
      subscription_price_usd:
        g.subscription_price_usd != null && g.subscription_price_usd > 0
          ? String(g.subscription_price_usd)
          : "",
    })
    setGroupDialogOpen(true)
  }

  const handleSaveGroup = async (e: React.FormEvent) => {
    e.preventDefault()
    setSavingGroup(true)
    try {
      const body: Record<string, unknown> = {
        name: groupForm.name,
        subscription_type: "subscription",
        rate_multiplier: parseFloat(groupForm.rate_multiplier) || 1.0,
        default_validity_days: parseInt(groupForm.default_validity_days) || 30,
      }
      if (groupForm.daily_limit_usd) body.daily_limit_usd = parseFloat(groupForm.daily_limit_usd)
      if (groupForm.weekly_limit_usd) body.weekly_limit_usd = parseFloat(groupForm.weekly_limit_usd)
      if (groupForm.monthly_limit_usd) body.monthly_limit_usd = parseFloat(groupForm.monthly_limit_usd)
      const selfPrice = parseFloat(groupForm.subscription_price_usd)
      body.subscription_price_usd = !Number.isFinite(selfPrice) || selfPrice < 0 ? 0 : selfPrice

      if (editingGroupId) {
        await fetchApi(`/admin/groups/${editingGroupId}`, {
          method: "PUT",
          body: JSON.stringify(body),
        })
        toast.success("订阅分组已更新")
      } else {
        await fetchApi("/admin/groups", {
          method: "POST",
          body: JSON.stringify(body),
        })
        toast.success("订阅分组创建成功")
      }
      await onSaved?.()
      setGroupDialogOpen(false)
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "保存失败")
    } finally {
      setSavingGroup(false)
    }
  }

  const handleDeleteGroup = async (g: Group) => {
    if (!confirm(`确定删除订阅分组「${g.name}」？关联的订阅将失效。`)) return
    try {
      await fetchApi(`/admin/groups/${g.id}`, { method: "DELETE" })
      toast.success("分组已删除")
      onSaved?.()
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "删除失败")
    }
  }

  return {
    groupDialogOpen,
    setGroupDialogOpen,
    editingGroupId,
    groupForm,
    setGroupForm,
    savingGroup,
    openCreateGroup,
    openEditGroup,
    handleSaveGroup,
    handleDeleteGroup,
  }
}
