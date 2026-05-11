import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/shared/components/ui/tabs'
import { Layers, CreditCard, Info, Calculator, Crown } from 'lucide-react'
import { useSearchParams } from 'react-router-dom'


import Pricing from '../pricing/AdminPricingPage'
import RedeemCodes from '../redeem-codes/AdminRedeemCodesPage'
import Subscriptions from '../subscriptions/AdminSubscriptionsPage'

const tabs = [
  { id: 'pricing', label: '分组倍率', icon: Layers },
  { id: 'redeem', label: '充值卡密', icon: CreditCard },
  { id: 'subscriptions', label: '订阅管理', icon: Crown },
]

export default function Billing() {
  const [searchParams, setSearchParams] = useSearchParams()
  const activeTab = searchParams.get('tab') || 'pricing'

  const handleTabChange = (value: string) => {
    setSearchParams({ tab: value }, { replace: true })
  }

  return (
    <div className="space-y-8 animate-in fade-in slide-in-from-bottom-4 duration-500 max-w-6xl mx-auto px-4 sm:px-6" style={{ willChange: 'transform, opacity' }}>
      <div className="flex flex-col gap-2">
        <h2 className="text-3xl font-extrabold tracking-tight text-gray-900 dark:text-white">计费管理引擎</h2>
        <p className="text-gray-500 dark:text-dark-300 text-lg">
          配置平台所有 AI 模型的结算价格、用户分组提权与充值系统。
        </p>
      </div>

      <div className="bg-gradient-to-br from-indigo-50 to-blue-50 dark:from-indigo-950/20 dark:to-blue-900/20 border border-indigo-100 dark:border-indigo-800/30 rounded-2xl p-6 shadow-sm relative overflow-hidden">
        <div className="absolute top-0 right-0 p-8 opacity-10 pointer-events-none">
          <Calculator className="w-32 h-32" />
        </div>
        <h3 className="text-lg font-bold text-indigo-900 dark:text-indigo-300 mb-4 flex items-center gap-2">
          <Info className="w-5 h-5" /> 计费系统是如何工作的？
        </h3>
        <div className="grid md:grid-cols-3 gap-6 relative z-10">
          <div className="space-y-2">
            <div className="font-semibold text-indigo-800 dark:text-indigo-400 flex items-center gap-2">
              <span className="bg-indigo-100 dark:bg-indigo-900 w-6 h-6 rounded-full flex items-center justify-center text-xs">1</span>
              设置基础定价
            </div>
            <p className="text-sm text-indigo-700/80 dark:text-indigo-300/80 leading-relaxed">
              前往左侧菜单的【模型】中配置各个上游模型（如 gpt-4）的基础价格，此价格以每 100 万 Token 的美元($)为标准。支持通配符匹配（如 <code className="bg-indigo-100 dark:bg-indigo-900/50 px-1 py-0.5 rounded text-xs font-mono">gpt-4-*</code>）和分组级别定价覆盖。定价匹配优先级：同分组精确 &gt; 同分组通配 &gt; 全局精确 &gt; 全局通配。
            </p>
          </div>
          <div className="space-y-2">
            <div className="font-semibold text-indigo-800 dark:text-indigo-400 flex items-center gap-2">
              <span className="bg-indigo-100 dark:bg-indigo-900 w-6 h-6 rounded-full flex items-center justify-center text-xs">2</span>
              分组倍率加成
            </div>
            <p className="text-sm text-indigo-700/80 dark:text-indigo-300/80 leading-relaxed">
              用户或 API Key 可以分配到特定分组。默认分组(default)倍率通常为 1。如果某分组倍率为 1.5，则该组用户消费将按基础价格的 1.5 倍扣款。<code className="bg-indigo-100 dark:bg-indigo-900/50 px-1 py-0.5 rounded text-xs font-mono">实际消耗 = 基础模型价 × 分组倍率</code>
            </p>
          </div>
          <div className="space-y-2">
            <div className="font-semibold text-indigo-800 dark:text-indigo-400 flex items-center gap-2">
              <span className="bg-indigo-100 dark:bg-indigo-900 w-6 h-6 rounded-full flex items-center justify-center text-xs">3</span>
              实时秒级扣费
            </div>
            <p className="text-sm text-indigo-700/80 dark:text-indigo-300/80 leading-relaxed">
              请求进来时先估算费用并冻结余额（预授权），请求完成后再根据真实 Token 量结算（多退少补），使用 MicroUSD（int64）整数精度避免浮点误差。后台每 60 秒扫描过期预授权（10分钟TTL）并恢复余额。Redis 不可用时自动降级为数据库直接扣减。
            </p>
          </div>
        </div>
      </div>

      <Tabs value={activeTab} onValueChange={handleTabChange} className="w-full">
        <TabsList className="relative flex w-full max-w-[600px] h-auto p-1.5 bg-gray-100/80 dark:bg-dark-800/80 rounded-full mb-6">
          {/* 滑动胶囊背景 */}
          <div
            className="absolute top-1.5 bottom-1.5 w-[calc((100%-12px)/3)] bg-white dark:bg-dark-700 rounded-full shadow-sm transition-transform duration-300 ease-out"
            style={{ 
              transform: `translateX(${tabs.findIndex(t => t.id === activeTab) * 100}%)`,
              left: '6px'
            }}
          />
          {tabs.map(tab => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              className="relative z-10 flex flex-1 items-center justify-center gap-2 py-3 px-4 text-sm font-medium text-gray-600 hover:text-gray-900 dark:text-dark-300 dark:hover:text-dark-50 data-[state=active]:text-gray-900 dark:data-[state=active]:text-white data-[state=active]:bg-transparent dark:data-[state=active]:bg-transparent data-[state=active]:shadow-none rounded-full transition-colors"
            >
              <tab.icon className="h-4 w-4" />
              {tab.label}
            </TabsTrigger>
          ))}
        </TabsList>

        <div className="bg-white dark:bg-dark-900 border border-gray-200 dark:border-dark-800 rounded-2xl p-6 shadow-sm min-h-[500px]">
          <TabsContent value="pricing" className="mt-0 focus-visible:outline-none">
            <Pricing />
          </TabsContent>
          <TabsContent value="redeem" className="mt-0 focus-visible:outline-none">
            <RedeemCodes />
          </TabsContent>
          <TabsContent value="subscriptions" className="mt-0 focus-visible:outline-none">
            <Subscriptions />
          </TabsContent>
        </div>
      </Tabs>
    </div>
  )
}
