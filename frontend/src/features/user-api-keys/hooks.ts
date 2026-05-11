import { useCallback, useEffect, useState } from "react"
import { errorMessage, fetchApi } from "@/shared/api/client"
import { toast } from "sonner"
import type { ApiKey, AvailableGroup, CreateKeyForm } from "./types"

interface ApiKeyListResponse {
  items: ApiKey[]
}

function isSubscriptionRequiredError(err: unknown): boolean {
  const msg = errorMessage(err, "")
  return msg.includes("需要先订阅该分组")
}

export function useApiKeys() {
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [loading, setLoading] = useState(true)
  const [copiedId, setCopiedId] = useState<number | null>(null)
  const [groups, setGroups] = useState<AvailableGroup[]>([])
  const [groupsLoading, setGroupsLoading] = useState(true)
  const [rebindingId, setRebindingId] = useState<number | null>(null)
  const [needSubDialog, setNeedSubDialog] = useState<{ open: boolean; groupName?: string }>({ open: false })

  const loadKeys = useCallback(async () => {
    try {
      const res = await fetchApi("/user/api-keys") as { data?: ApiKeyListResponse }
      setKeys(res.data?.items ?? [])
    } catch (err: unknown) {
      toast.error(errorMessage(err, "无法加载 API Keys"))
    } finally {
      setLoading(false)
    }
  }, [])

  const loadGroups = useCallback(async () => {
    try {
      const res = await fetchApi("/user/available-groups")
      setGroups(res.data || [])
    } catch {
      // silent fail for groups
    } finally {
      setGroupsLoading(false)
    }
  }, [])

  useEffect(() => {
    loadGroups()
  }, [loadGroups])

  const handleCreate = async (
    form: CreateKeyForm,
    onSuccess: () => void
  ) => {
    if (!form.name.trim()) return
    try {
      const payload: Record<string, string | number | null> = { name: form.name.trim() }
      if (form.quota) payload.quota = parseFloat(form.quota)
      if (form.rate_5h) payload.rate_limit_5h = parseFloat(form.rate_5h)
      if (form.rate_1d) payload.rate_limit_1d = parseFloat(form.rate_1d)
      if (form.rate_7d) payload.rate_limit_7d = parseFloat(form.rate_7d)
      if (form.rate_30d) payload.rate_limit_30d = parseFloat(form.rate_30d)
      if (form.group_id) payload.group_id = form.group_id
      if (form.expires_in_days !== undefined) payload.expires_in_days = form.expires_in_days

      await fetchApi("/user/api-keys", {
        method: "POST",
        body: JSON.stringify(payload),
      })
      toast.success("成功创建 API Key")
      onSuccess()
      await loadKeys()
    } catch (err: unknown) {
      if (isSubscriptionRequiredError(err)) {
        const gid = form.group_id
        const gname = gid ? groups.find((g) => g.id === gid)?.name : undefined
        setNeedSubDialog({ open: true, groupName: gname })
        return
      }
      toast.error(errorMessage(err, "创建失败"))
    }
  }

  const handleDelete = async (id: number) => {
    if (!confirm("确定要删除这个 API Key 吗？此操作不可撤销。")) return
    try {
      await fetchApi(`/user/api-keys/${id}`, { method: "DELETE" })
      toast.success("API Key 已删除")
      await loadKeys()
    } catch (err: unknown) {
      toast.error(errorMessage(err, "删除失败"))
    }
  }

  const handleCopy = (id: number, keyStr: string) => {
    navigator.clipboard.writeText(keyStr)
    setCopiedId(id)
    setTimeout(() => setCopiedId(null), 2000)
    toast.success("已复制到剪贴板")
  }

  const handleRebindGroup = async (keyId: number, groupId: number | null) => {
    setRebindingId(keyId)
    try {
      await fetchApi(`/user/api-keys/${keyId}/group`, {
        method: "PATCH",
        body: JSON.stringify({ group_id: groupId }),
      })
      toast.success(groupId ? "已切换分组" : "已解绑分组")
      await loadKeys()
    } catch (err: unknown) {
      if (isSubscriptionRequiredError(err)) {
        const gname = groupId != null ? groups.find((g) => g.id === groupId)?.name : undefined
        setNeedSubDialog({ open: true, groupName: gname })
        return
      }
      toast.error(errorMessage(err, "切换分组失败"))
    } finally {
      setRebindingId(null)
    }
  }

  return {
    keys,
    loading,
    copiedId,
    groups,
    groupsLoading,
    rebindingId,
    needSubDialog,
    setNeedSubDialog,
    loadKeys,
    handleCreate,
    handleDelete,
    handleCopy,
    handleRebindGroup,
  }
}
