import { useEffect, useState } from 'react'
import { fetchApi, isAbortError } from '@/shared/api/client'
import { useAuthStore } from '@/features/auth/auth_store'
import type {
  DashboardStats,
  UsageStats,
  AnnouncementItem,
  TrendPoint,
  ModelStat,
  RecentUsage,
  IntegrationTab,
} from '@/features/user-dashboard/types'
import { DashboardAnnouncements } from '@/features/user-dashboard/components/DashboardAnnouncements'
import { AdminDashboardOverview } from '@/features/user-dashboard/components/AdminDashboardOverview'
import { AdminDashboardCharts } from '@/features/user-dashboard/components/AdminDashboardCharts'
import { UserDashboardHero } from '@/features/user-dashboard/components/UserDashboardHero'
import { UserDashboardCharts } from '@/features/user-dashboard/components/UserDashboardCharts'
import { RecentUsageTable } from '@/features/user-dashboard/components/RecentUsageTable'
import { QuickIntegrationPanel } from '@/features/user-dashboard/components/QuickIntegrationPanel'

export default function Dashboard() {
  const user = useAuthStore(s => s.user)
  const isAdmin = user?.role === 'admin'
  const [stats, setStats] = useState<DashboardStats | null>(null)
  const [usageStats, setUsageStats] = useState<UsageStats | null>(null)
  const [loading, setLoading] = useState(true)
  const [announcements, setAnnouncements] = useState<AnnouncementItem[]>([])
  const [integrationTab, setIntegrationTab] = useState<IntegrationTab>('openai')

  // Phase 2: Chart data
  const [trendData, setTrendData] = useState<TrendPoint[]>([])
  const [modelData, setModelData] = useState<ModelStat[]>([])
  const [recentUsage, setRecentUsage] = useState<RecentUsage[]>([])
  const [trendDays, setTrendDays] = useState<7 | 30>(7)

  useEffect(() => {
    const controller = new AbortController()
    const { signal } = controller

    const loadStats = async () => {
      try {
        if (isAdmin) {
          // Fire all admin requests in parallel
          const [dashboardRes, trendRes, modelRes, annRes] = await Promise.all([
            fetchApi('/admin/dashboard', { signal }),
            fetchApi(`/admin/usage/trend?days=${trendDays}`, { signal }).catch(err => {
              if (!signal.aborted && !isAbortError(err)) console.error("Failed to fetch admin chart data")
              return null
            }),
            fetchApi('/admin/usage/models?days=30', { signal }).catch(err => {
              if (!signal.aborted && !isAbortError(err)) console.error("Failed to fetch admin chart data")
              return null
            }),
            fetchApi('/user/announcements', { signal }).catch(() => null),
          ])
          if (signal.aborted) return
          setStats(dashboardRes.data)
          if (trendRes) setTrendData(trendRes.data || [])
          if (modelRes) setModelData(modelRes.data || [])
          if (annRes) setAnnouncements(annRes.data || [])
        } else {
          // Fire all user requests in parallel
          const [profileRes, usageRes, trendRes, modelRes, recentRes, annRes] = await Promise.all([
            fetchApi('/user/profile', { signal }),
            fetchApi('/user/usage/stats', { signal }).catch(err => {
              if (!signal.aborted && !isAbortError(err)) console.error("Failed to fetch user usage stats")
              return null
            }),
            fetchApi(`/user/usage/trend?days=${trendDays}`, { signal }).catch(err => {
              if (!signal.aborted && !isAbortError(err)) console.error("Failed to fetch chart data")
              return null
            }),
            fetchApi('/user/usage/models?days=30', { signal }).catch(() => null),
            fetchApi('/user/usage/detail?page=1&page_size=5', { signal }).catch(() => null),
            fetchApi('/user/announcements', { signal }).catch(() => null),
          ])
          if (signal.aborted) return
          const ustats = usageRes?.data?.usage ?? null
          const userBalance = profileRes.data.available_balance ?? profileRes.data.user?.balance ?? 0
          setStats({
            users: { total: 1, active: 1 },
            api_keys: { total: profileRes.data.key_count, active: profileRes.data.key_count },
            usage: { today_requests: ustats?.total_requests || 0, today_cost: userBalance, week_requests: 0 },
            isUser: true,
            balance: userBalance,
            quota: profileRes.data.quota || 0,
            used_quota: profileRes.data.used_quota || 0,
          })
          setUsageStats(ustats)
          if (trendRes) setTrendData(trendRes.data || [])
          if (modelRes) setModelData(modelRes.data || [])
          if (recentRes) setRecentUsage(recentRes.data?.items || [])
          if (annRes) setAnnouncements(annRes.data || [])
        }
      } catch (err) {
        if (!signal.aborted && !isAbortError(err)) console.error(err)
      } finally {
        if (!signal.aborted) setLoading(false)
      }
    }
    void loadStats()

    return () => { controller.abort() }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isAdmin])

  // Reload trend when days change
  useEffect(() => {
    const controller = new AbortController()

    const url = isAdmin
      ? `/admin/usage/trend?days=${trendDays}`
      : `/user/usage/trend?days=${trendDays}`

    fetchApi(url, { signal: controller.signal }).then(res => {
      if (!controller.signal.aborted) setTrendData(res.data || [])
    }).catch(() => {})

    return () => { controller.abort() }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [trendDays])

  if (loading) {
    return <DashboardSkeleton />
  }

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500">
      <DashboardAnnouncements announcements={announcements} />

      {/* Admin Dashboard */}
      {isAdmin && (
        <>
          <AdminDashboardOverview stats={stats} />
          <AdminDashboardCharts
            trendData={trendData}
            modelData={modelData}
            trendDays={trendDays}
            onTrendDaysChange={setTrendDays}
          />
        </>
      )}

      {/* User Dashboard */}
      {!isAdmin && (
        <div className="space-y-8">
          <UserDashboardHero email={user?.email} stats={stats} usageStats={usageStats} />
          <UserDashboardCharts
            trendData={trendData}
            modelData={modelData}
            trendDays={trendDays}
            onTrendDaysChange={setTrendDays}
          />
          <div className="grid gap-6 lg:grid-cols-2">
            <RecentUsageTable recentUsage={recentUsage} />
            <QuickIntegrationPanel
              apiKeyCount={stats?.api_keys?.total || 0}
              integrationTab={integrationTab}
              onIntegrationTabChange={setIntegrationTab}
            />
          </div>
        </div>
      )}
    </div>
  )
}

function DashboardSkeleton() {
  return (
    <div
      aria-busy="true"
      aria-label="Loading dashboard"
      className="space-y-8 animate-pulse"
      role="status"
    >
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {Array.from({ length: 4 }).map((_, index) => (
          <div
            className="rounded-2xl border border-border bg-card p-5 shadow-sm dark:border-dark-800 dark:bg-dark-900"
            key={index}
          >
            <div className="mb-5 flex items-center justify-between">
              <div className="h-4 w-24 rounded bg-muted dark:bg-dark-800" />
              <div className="h-10 w-10 rounded-xl bg-muted dark:bg-dark-800" />
            </div>
            <div className="h-7 w-28 rounded bg-muted dark:bg-dark-800" />
            <div className="mt-3 h-3 w-32 rounded bg-muted dark:bg-dark-800" />
          </div>
        ))}
      </div>
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="rounded-2xl border border-border bg-card p-6 shadow-sm dark:border-dark-800 dark:bg-dark-900 lg:col-span-2">
          <div className="mb-6 flex items-center justify-between">
            <div className="h-5 w-40 rounded bg-muted dark:bg-dark-800" />
            <div className="h-9 w-28 rounded-full bg-muted dark:bg-dark-800" />
          </div>
          <div className="flex h-64 items-end gap-3">
            {[35, 58, 44, 72, 55, 88, 64].map(height => (
              <div className="flex-1 rounded-t-lg bg-muted dark:bg-dark-800" key={height} style={{ height: `${height}%` }} />
            ))}
          </div>
        </div>
        <div className="space-y-4 rounded-2xl border border-border bg-card p-6 shadow-sm dark:border-dark-800 dark:bg-dark-900">
          <div className="h-5 w-36 rounded bg-muted dark:bg-dark-800" />
          {Array.from({ length: 5 }).map((_, index) => (
            <div className="flex items-center gap-3" key={index}>
              <div className="h-9 w-9 rounded-full bg-muted dark:bg-dark-800" />
              <div className="flex-1 space-y-2">
                <div className="h-3 w-3/4 rounded bg-muted dark:bg-dark-800" />
                <div className="h-3 w-1/2 rounded bg-muted dark:bg-dark-800" />
              </div>
            </div>
          ))}
        </div>
      </div>
    </div>
  )
}
