import { Copy, Check, Trash2 } from "lucide-react"
import { Button } from "@/shared/components/ui/button"
import { ApiKeyStatusBadge } from "./ApiKeyStatusBadge"
import { ApiKeyUsageDialog } from "./ApiKeyUsageDialog"
import { ModelListDialog } from "./ModelListDialog"
import { QuotaProgressBar } from "./QuotaProgressBar"
import { ExpirationCountdown } from "./ExpirationCountdown"
import { GroupRebindDropdown } from "./GroupRebindDropdown"
import type { ApiKey, AvailableGroup } from "../types"

function maskApiKeyDisplay(key: string): string {
  if (key.startsWith("sk-cpa-")) return "sk-cpa-****"
  if (key.startsWith("sk-")) return "sk-****"
  return "****"
}

interface Props {
  keys: ApiKey[]
  loading: boolean
  copiedId: number | null
  onCopy: (id: number, key: string) => void
  onDelete: (id: number) => void
  groups: AvailableGroup[]
  groupsLoading: boolean
  rebindingId: number | null
  onRebindGroup: (keyId: number, groupId: number | null) => void
}

export function ApiKeysTable({
  keys,
  loading,
  copiedId,
  onCopy,
  onDelete,
  groups,
  groupsLoading,
  rebindingId,
  onRebindGroup,
}: Props) {
  return (
    <div className="glass-card overflow-hidden">
      <div className="overflow-x-auto">
        <table className="table">
          <thead>
            <tr>
              <th className="w-[180px]">调用凭证名称</th>
              <th>API Key (点击复制)</th>
              <th>状态</th>
              <th className="hidden md:table-cell">分组 / 额度</th>
              <th className="hidden lg:table-cell">有效期</th>
              <th className="hidden lg:table-cell">最近使用</th>
              <th>操作</th>
              <th className="w-[50px]"></th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={8} className="h-32 text-center text-gray-500">
                  <div className="flex justify-center items-center gap-2">
                    <div className="w-4 h-4 rounded-full bg-primary-500/50 animate-pulse" />
                    数据加载中...
                  </div>
                </td>
              </tr>
            ) : keys.length === 0 ? (
              <tr>
                <td colSpan={8} className="h-32 text-center text-gray-500">
                  您还没有创建任何 API Key，请点击上方按钮新建。
                </td>
              </tr>
            ) : (
              keys.map((k) => (
                <tr key={k.id}>
                  {/* Name + group badge (mobile) */}
                  <td className="font-medium text-gray-900 dark:text-white">
                    <div className="flex flex-col gap-1">
                      <span>{k.name}</span>
                      {/* Mobile-only group name */}
                      {k.group_name && (
                        <span className="md:hidden inline-flex w-fit items-center rounded-md border border-primary-500/20 bg-primary-50 dark:bg-primary-900/20 px-2 py-0.5 text-xs font-semibold text-primary-600 dark:text-primary-400">
                          {k.group_name}
                        </span>
                      )}
                    </div>
                  </td>

                  {/* Key snippet with copy */}
                  <td>
                    <div
                      className="font-mono text-xs bg-gray-100 dark:bg-dark-900 hover:bg-gray-200 dark:hover:bg-dark-700 text-gray-700 dark:text-gray-300 py-1.5 px-2.5 rounded-lg border border-border cursor-pointer inline-flex items-center gap-2 group transition-colors"
                      onClick={() => onCopy(k.id, k.key)}
                    >
                      {maskApiKeyDisplay(k.key)}
                      {copiedId === k.id ? (
                        <Check className="h-3.5 w-3.5 text-green-500" />
                      ) : (
                        <Copy className="h-3.5 w-3.5 opacity-50 group-hover:opacity-100 transition-opacity" />
                      )}
                    </div>
                  </td>

                  {/* Status badge */}
                  <td>
                    <ApiKeyStatusBadge status={k.display_status || k.status} />
                  </td>

                  {/* Group rebind + quota + rate limit (md+) */}
                  <td className="hidden md:table-cell">
                    <div className="flex flex-col gap-2 min-w-[180px]">
                      <GroupRebindDropdown
                        currentGroupId={k.group_id}
                        currentGroupName={k.group_name}
                        groups={groups}
                        loading={groupsLoading}
                        onRebind={(groupId) => onRebindGroup(k.id, groupId)}
                        rebinding={rebindingId === k.id}
                      />
                      {k.quota > 0 && (
                        <QuotaProgressBar
                          used={k.quota_used}
                          total={k.quota}
                          label="总额度"
                        />
                      )}
                      {k.rate_limit_30d > 0 && (
                        <QuotaProgressBar
                          used={k.usage_30d}
                          total={k.rate_limit_30d}
                          label="月限额"
                        />
                      )}
                    </div>
                  </td>

                  {/* Expiration countdown (lg+) */}
                  <td className="hidden lg:table-cell">
                    <ExpirationCountdown expiresAt={k.expires_at} />
                  </td>

                  {/* Last used (lg+) */}
                  <td className="hidden lg:table-cell text-sm text-gray-500 dark:text-gray-400 font-mono">
                    {k.last_used_at
                      ? new Date(k.last_used_at).toLocaleDateString()
                      : "—"}
                  </td>

                  {/* Actions: Details + Models */}
                  <td>
                    <div className="flex items-center gap-1.5">
                      <ApiKeyUsageDialog key_={k} />
                      <ModelListDialog key_={k} />
                    </div>
                  </td>

                  {/* Delete */}
                  <td className="text-right">
                    <Button
                      type="button"
                      variant="dangerIcon"
                      onClick={() => onDelete(k.id)}
                      title="删除凭证"
                      aria-label="删除凭证"
                    >
                      <Trash2 />
                    </Button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
