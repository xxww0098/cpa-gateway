import React, { useState } from 'react'
import { useNavigate, Link } from 'react-router-dom'
import { fetchApi } from '@/shared/api/client'
import { Mail, Lock, ArrowRight, Loader2, AlertCircle, Key, Eye, EyeOff } from 'lucide-react'
import { toast } from 'sonner'

export default function Register() {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [inviteCode, setInviteCode] = useState('')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const navigate = useNavigate()

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    try {
      const res = await fetchApi('/auth/register', {
        method: 'POST',
        body: JSON.stringify({ email, password, invite_code: inviteCode })
      })
      if (res.code === 0) {
        toast.success("注册成功，请登录")
        navigate('/login')
      } else {
        setError(res.message || '注册失败')
      }
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : '请求异常')
    } finally {
      setLoading(false)
    }
  }

  return (
    <>
      <div className="text-center mb-8">
        <h2 className="text-2xl font-bold text-gray-900 dark:text-white mb-2">创建新账户</h2>
        <p className="text-gray-500 dark:text-gray-400">加入平台，管理您的 API 服务</p>
      </div>

      <form onSubmit={handleSubmit} className="space-y-5">
        {error && (
          <div className="flex items-center gap-2 p-3 rounded-xl bg-red-50 dark:bg-red-900/20 text-red-600 dark:text-red-400 border border-red-100 dark:border-red-900/50 animate-in fade-in slide-in-from-top-2">
            <AlertCircle className="w-5 h-5 flex-shrink-0" />
            <span className="text-sm font-medium">{error}</span>
          </div>
        )}

        <div className="space-y-1">
          <label className="input-label">邮箱</label>
          <div className="relative">
            <div className="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none text-gray-400">
              <Mail className="h-5 w-5" />
            </div>
            <input
              type="email"
              className="input pl-10"
              placeholder="yours@example.com"
              value={email}
              onChange={e => setEmail(e.target.value)}
              required
            />
          </div>
        </div>

        <div className="space-y-1">
          <label className="input-label">设置密码</label>
          <div className="relative">
            <div className="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none text-gray-400">
              <Lock className="h-5 w-5" />
            </div>
            <input
              type={showPassword ? "text" : "password"}
              className="input pl-10 pr-10"
              placeholder="至少 8 位"
              value={password}
              onChange={e => setPassword(e.target.value)}
              required
              minLength={8}
            />
            <button
              type="button"
              onClick={() => setShowPassword(!showPassword)}
              className="absolute inset-y-0 right-0 pr-3 flex items-center text-gray-400 hover:text-gray-600 dark:hover:text-gray-300 transition-colors"
            >
              {showPassword ? <EyeOff className="h-5 w-5" /> : <Eye className="h-5 w-5" />}
            </button>
          </div>
        </div>

        <div className="space-y-1">
          <label className="input-label">邀请码 (可选)</label>
          <div className="relative">
            <div className="absolute inset-y-0 left-0 pl-3 flex items-center pointer-events-none text-gray-400">
              <Key className="h-5 w-5" />
            </div>
            <input
              type="text"
              className="input pl-10"
              placeholder="如果您有邀请码"
              value={inviteCode}
              onChange={e => setInviteCode(e.target.value)}
            />
          </div>
        </div>

        <button 
          type="submit" 
          disabled={loading || !email || !password}
          className="btn btn-primary w-full mt-2"
        >
          {loading ? (
            <><Loader2 className="w-5 h-5 animate-spin mr-2" /> 提交中...</>
          ) : (
            <>立即注册 <ArrowRight className="w-4 h-4 ml-1" /></>
          )}
        </button>
      </form>

      <div className="mt-6 text-center text-sm text-gray-600 dark:text-gray-400">
        已有账户?{' '}
        <Link to="/login" className="font-semibold text-primary-600 hover:text-primary-500 transition-colors">
          返回登录
        </Link>
      </div>
    </>
  )
}
