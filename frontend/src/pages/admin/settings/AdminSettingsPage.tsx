import { useState } from 'react'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/shared/components/ui/tabs'
import { Settings as SettingsIcon, ScrollText, Bell, MessageSquareText } from 'lucide-react'
import { useSearchParams } from 'react-router-dom'

import AdminProxyConfigPage from '../proxy/AdminProxyConfigPage'
import AdminProxyLogsPage from '../proxy/AdminProxyLogsPage'
import Announcements from '../announcements/AdminAnnouncementsPage'
import AdminTicketQuickRepliesPage from './AdminTicketQuickRepliesPage'

const tabs = [
  { id: 'config', label: '网关配置', icon: SettingsIcon },
  { id: 'logs', label: '运行日志', icon: ScrollText },
  { id: 'announcements', label: '系统公告', icon: Bell },
  { id: 'ticket-replies', label: '工单快捷回复', icon: MessageSquareText },
]

export default function AdminSettings() {
  const [searchParams, setSearchParams] = useSearchParams()
  const initialTab = searchParams.get('tab') || 'config'
  const [activeTab, setActiveTab] = useState(initialTab)

  const handleTabChange = (value: string) => {
    setActiveTab(value)
    setSearchParams({ tab: value }, { replace: true })
  }

  return (
    <div className="space-y-6 animate-in fade-in slide-in-from-bottom-4 duration-500 max-w-7xl mx-auto" style={{ willChange: 'transform, opacity' }}>
      <div>
        <h2 className="text-2xl font-bold tracking-tight text-gray-900 dark:text-white">系统设置</h2>
        <p className="text-gray-500 dark:text-dark-300 mt-1">
          网关运行参数、日志、公告与工单客服快捷回复。
        </p>
      </div>

      <Tabs value={activeTab} onValueChange={handleTabChange} className="w-full">
        <TabsList className="grid h-auto w-full max-w-3xl grid-cols-2 gap-1 p-1 sm:grid-cols-4 bg-gray-100/80 dark:bg-dark-800/80 rounded-xl">
          {tabs.map(tab => (
            <TabsTrigger
              key={tab.id}
              value={tab.id}
              className="flex items-center gap-2 py-2.5 px-3 text-sm font-medium data-[state=active]:bg-white dark:data-[state=active]:bg-dark-700 data-[state=active]:shadow-sm rounded-lg transition-all"
            >
              <tab.icon className="h-4 w-4" />
              {tab.label}
            </TabsTrigger>
          ))}
        </TabsList>

        <TabsContent value="config" className="mt-6 focus-visible:outline-none">
          <AdminProxyConfigPage />
        </TabsContent>
        <TabsContent value="logs" className="mt-6 focus-visible:outline-none">
          <AdminProxyLogsPage />
        </TabsContent>
        <TabsContent value="announcements" className="mt-6 focus-visible:outline-none">
          <Announcements />
        </TabsContent>
        <TabsContent value="ticket-replies" className="mt-6 focus-visible:outline-none">
          <AdminTicketQuickRepliesPage />
        </TabsContent>
      </Tabs>
    </div>
  )
}
