import { Navigate } from 'react-router-dom'
import { useAuthStore } from '@/features/auth/auth_store'

/** 路由守卫：需要管理员角色才能访问包裹的路由。 */
export function RequireAdmin({ children }: { children: React.ReactNode }) {
  const user = useAuthStore(s => s.user)
  const token = useAuthStore(s => s.token)

  if (!token || !user) {
    return <Navigate to="/login" replace />
  }

  if (user.role !== 'admin') {
    return <Navigate to="/dashboard" replace />
  }

  return <>{children}</>
}
