/**
 * Auth feature module type definitions.
 * Aligned with backend handler_auth.go and handler_user.go response shapes.
 */

// ── Request Types ───────────────────────────────────────────────────────────

/** POST /auth/login request body */
export interface LoginRequest {
  email: string
  password: string
}

/** POST /auth/register request body */
export interface RegisterRequest {
  email: string
  password: string
  invite_code?: string
}

// ── Response Types ──────────────────────────────────────────────────────────

/** User object returned by login/register/profile endpoints (matches authUserResponse in backend) */
export interface AuthUser {
  id: number
  email: string
  role: 'user' | 'admin'
  balance: number
  status: string
  created_at: string
}

/** POST /auth/login and POST /auth/register response data */
export interface AuthResponse {
  token: string
  user: AuthUser
}

/** GET /user/profile response data */
export interface ProfileResponse {
  user: AuthUser
  available_balance: number
}
