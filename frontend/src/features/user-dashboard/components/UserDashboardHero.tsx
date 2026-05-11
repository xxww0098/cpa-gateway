import { Activity, Cpu, DollarSign } from 'lucide-react'
import { ProgressRing } from '@/shared/components/ui/ProgressRing'
import type { UserDashboardHeroProps } from '../types'

export function UserDashboardHero({ email, stats, usageStats }: UserDashboardHeroProps) {
  return (
    <div className="space-y-8">
      <div className="mb-8">
        <h2 className="text-2xl font-bold tracking-tight text-gray-900 dark:text-white">欢迎，{email}</h2>
        <p className="text-gray-500 dark:text-dark-400">以下是您的额度使用情况及接口状态</p>
      </div>

      {/* Progress Rings */}
      <div className="grid gap-6 md:grid-cols-3">
        {/* Balance Ring */}
        <div className="glass-card overflow-hidden">
          <div className="px-6 pt-6 pb-4 border-b border-border/50">
            <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500 dark:text-dark-300 flex justify-between items-center">
              可用余额
              <DollarSign className="w-5 h-5 opacity-50" />
            </h3>
          </div>
          <div className="p-6 flex justify-center pb-8">
            <ProgressRing
              percentage={100}
              value={`$${(stats?.balance || 0).toFixed(2)}`}
              label="当前可用"
              gradientFrom="#14b8a6"
              gradientTo="#0d9488"
            />
          </div>
        </div>

        {/* Total Requests */}
        <div className="glass-card overflow-hidden">
          <div className="px-6 pt-6 pb-4 border-b border-border/50">
            <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500 dark:text-dark-300 flex justify-between items-center">
              总请求次数
              <Activity className="w-5 h-5 opacity-50" />
            </h3>
          </div>
          <div className="p-6 flex justify-center pb-8">
            <ProgressRing
              percentage={usageStats?.success_count
                ? (usageStats.success_count / (usageStats.total_requests || 1)) * 100
                : 100}
              value={`${usageStats?.total_requests || 0}`}
              label="请求总数"
              subValue={`${usageStats?.success_count || 0} 成功`}
              gradientFrom="#3b82f6"
              gradientTo="#6366f1"
            />
          </div>
        </div>

        {/* Total Tokens */}
        <div className="glass-card overflow-hidden">
          <div className="px-6 pt-6 pb-4 border-b border-border/50">
            <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500 dark:text-dark-300 flex justify-between items-center">
              消耗 Tokens
              <Cpu className="w-5 h-5 opacity-50" />
            </h3>
          </div>
          <div className="p-6 flex justify-center pb-8">
            <ProgressRing
              percentage={75}
              value={`${(usageStats?.total_tokens || 0).toLocaleString()}`}
              label="处理 Tokens"
              gradientFrom="#f59e0b"
              gradientTo="#f97316"
            />
          </div>
        </div>
      </div>
    </div>
  )
}
