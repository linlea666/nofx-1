import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

// Signed-out visitor: no user, no token.
vi.mock('./contexts/AuthContext', () => ({
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
  useAuth: () => ({
    user: null,
    token: null,
    logout: vi.fn(),
    isLoading: false,
  }),
}))
vi.mock('./hooks/useSystemConfig', () => ({
  useSystemConfig: () => ({ loading: false }),
}))
vi.mock('./components/LoginPage', () => ({
  LoginPage: () => <div>login-page</div>,
}))
vi.mock('./pages/LandingPage', () => ({
  LandingPage: () => <div>landing-page</div>,
}))
vi.mock('./components/CompetitionPage', () => ({
  CompetitionPage: () => <div>competition-page</div>,
}))
vi.mock('./components/RegisterPage', () => ({
  RegisterPage: () => <div>register-page</div>,
}))

import App from './App'

afterEach(cleanup)

describe('private deployment gate', () => {
  for (const path of ['/', '/competition', '/faq', '/backtest', '/dashboard']) {
    it(`shows the login page instead of ${path} when signed out`, () => {
      window.history.pushState({}, '', path)
      render(<App />)
      expect(screen.getByText('login-page')).toBeInTheDocument()
      expect(screen.queryByText('landing-page')).not.toBeInTheDocument()
      expect(screen.queryByText('competition-page')).not.toBeInTheDocument()
    })
  }
  it('keeps registration reachable when signed out', () => {
    window.history.pushState({}, '', '/register')
    render(<App />)
    expect(screen.getByText('register-page')).toBeInTheDocument()
  })
})
