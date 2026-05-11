import { useState, useEffect } from "react"
import { fetchApi } from "@/shared/api/client"
import { Card } from "@/shared/components/ui/card"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/shared/components/ui/table"
import { Badge } from "@/shared/components/ui/badge"
import { toast } from "sonner"
import { ShieldAlert } from "lucide-react"

interface AuditLog {
  id: number
  user_id: number
  action: string
  target_resource?: string
  created_at: string
}

export default function AuditLogs() {
  const [logs, setLogs] = useState<AuditLog[]>([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    loadLogs()
  }, [])

  const loadLogs = async () => {
    setLoading(true)
    try {
      const res = await fetchApi(`/admin/audit-logs`)
      setLogs(res.data.items || [])
    } catch (err: unknown) {
      toast.error(err instanceof Error ? err.message : "加载失败")
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col sm:flex-row justify-between items-start sm:items-center gap-4">
        <div>
          <h2 className="text-3xl font-bold tracking-tight text-foreground flex items-center gap-2">
            <ShieldAlert className="h-8 w-8 text-primary" />
            系统操作日志
          </h2>
          <p className="text-muted-foreground mt-1">管理员的关键操作审计流水线记录 (M5)</p>
        </div>
      </div>

      <Card className="shadow-sm border-border overflow-hidden">
        <Table>
          <TableHeader className="bg-secondary/50">
            <TableRow>
              <TableHead>触发人员</TableHead>
              <TableHead>行为动作</TableHead>
              <TableHead>受影响资产</TableHead>
              <TableHead className="text-right">发生时间</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow>
                <TableCell colSpan={4} className="h-32 text-center text-muted-foreground">流水拉取中...</TableCell>
              </TableRow>
            ) : logs.length === 0 ? (
              <TableRow>
                <TableCell colSpan={4} className="h-32 text-center text-muted-foreground">毫无破绽，空空如也。</TableCell>
              </TableRow>
            ) : (
              logs.map((l) => (
                <TableRow key={l.id}>
                  <TableCell className="font-medium text-sm">
                    UID: {l.user_id}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className="font-mono text-xs">{l.action}</Badge>
                  </TableCell>
                  <TableCell className="text-sm font-mono text-muted-foreground">
                    {l.target_resource || "-"}
                  </TableCell>
                  <TableCell className="text-right text-muted-foreground text-sm">
                    {new Date(l.created_at).toLocaleString()}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  )
}
