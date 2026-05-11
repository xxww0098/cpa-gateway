import { Outlet } from 'react-router-dom'

export function AuthLayout() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-50 dark:bg-gray-950 p-4">
      <div className="w-full max-w-md animate-in fade-in zoom-in-95 duration-500" style={{ willChange: 'transform, opacity' }}>
        {/* Logo or Brand */}
        <div className="flex justify-center mb-8 shrink-0">
          <div className="w-14 h-14 rounded-2xl bg-gradient-to-br from-primary-500 to-primary-600 shadow-glow flex items-center justify-center">
            <span className="text-2xl font-bold text-white">C</span>
          </div>
        </div>

        {/* Card Content Container */}
        <div className="glass-card w-full shadow-2xl p-8">
          <Outlet />
        </div>
        
        {/* Footer info inside auth layout typically */}
        <div className="mt-8 text-center text-sm text-gray-500 dark:text-dark-400">
          &copy; {new Date().getFullYear()} CPA Gateway. All rights reserved.
        </div>
      </div>
    </div>
  )
}
