import type {
  SystemStatus,
  AccountInfo,
  Position,
  DecisionRecord,
  Statistics,
  TraderInfo,
  TraderConfigData,
  AIModel,
  Exchange,
  CreateTraderRequest,
  CreateExchangeRequest,
  UpdateModelConfigRequest,
  UpdateExchangeConfigRequest,
  CompetitionData,
  BacktestRunsResponse,
  BacktestStartConfig,
  BacktestStatusPayload,
  BacktestEquityPoint,
  BacktestTradeEvent,
  BacktestMetrics,
  BacktestRunMetadata,
  Strategy,
  StrategyConfig,
  DebateSession,
  DebateSessionWithDetails,
  CreateDebateRequest,
  DebateMessage,
  DebateVote,
  DebatePersonalityInfo,
  BinanceCredentialsView,
  BinanceCredentialsListResponse,
  BinanceCredentialsSetRequest,
  BinanceCredentialsTestResponse,
  BinanceCredentialsAffectedResponse,
  CopyGuardAccountRiskPolicyResponse,
} from '../types'
import { CryptoService } from './crypto'
import { httpClient } from './httpClient'

const API_BASE = '/api'

// Helper function to get auth headers
function getAuthHeaders(): Record<string, string> {
  const token = localStorage.getItem('auth_token')
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }

  if (token) {
    headers['Authorization'] = `Bearer ${token}`
  }

  return headers
}

async function handleJSONResponse<T>(res: Response): Promise<T> {
  const text = await res.text()
  if (!res.ok) {
    let message = text || res.statusText
    try {
      const data = text ? JSON.parse(text) : null
      if (data && typeof data === 'object') {
        message = data.error || data.message || message
      }
    } catch {
      /* ignore JSON parse errors */
    }
    throw new Error(message || '请求失败')
  }
  if (!text) {
    return {} as T
  }
  return JSON.parse(text) as T
}

export const api = {
  // AI交易员管理接口
  async getTraders(): Promise<TraderInfo[]> {
    const result = await httpClient.get<TraderInfo[]>(`${API_BASE}/my-traders`)
    if (!result.success) throw new Error('获取trader列表失败')
    return Array.isArray(result.data) ? result.data : []
  },

  // 获取公开的交易员列表（无需认证）
  async getPublicTraders(): Promise<any[]> {
    const result = await httpClient.get<any[]>(`${API_BASE}/traders`)
    if (!result.success) throw new Error('获取公开trader列表失败')
    return result.data!
  },

  async createTrader(request: CreateTraderRequest): Promise<TraderInfo> {
    const result = await httpClient.post<TraderInfo>(
      `${API_BASE}/traders`,
      request
    )
    if (!result.success) throw new Error('创建交易员失败')
    return result.data!
  },

  async deleteTrader(traderId: string): Promise<{
    status: string
    message: string
    pending_blockers?: TraderInfo['pending_blockers']
  }> {
    const result = await httpClient.delete<{
      status: string
      message: string
      pending_blockers?: TraderInfo['pending_blockers']
    }>(`${API_BASE}/traders/${traderId}`)
    if (!result.success) {
      const blockers = result.data?.pending_blockers || []
      const details = blockers
        .map(
          (blocker) =>
            `${blocker.code}:${blocker.symbol || blocker.resource_id}(${blocker.status})`
        )
        .join('；')
      throw new Error(
        details
          ? `${result.message || '归档交易员失败'}：${details}`
          : result.message || '归档交易员失败'
      )
    }
    return result.data!
  },

  async startTrader(traderId: string): Promise<{
    status: string
    lifecycle_generation: number
    message: string
  }> {
    const result = await httpClient.post<{
      status: string
      lifecycle_generation: number
      message: string
    }>(`${API_BASE}/traders/${traderId}/start`)
    if (!result.success) throw new Error(result.message || '启动交易员失败')
    return result.data!
  },

  async stopTrader(traderId: string): Promise<{
    status: string
    lifecycle_generation: number
    pending_blockers: TraderInfo['pending_blockers']
    message: string
  }> {
    const result = await httpClient.post<{
      status: string
      lifecycle_generation: number
      pending_blockers: TraderInfo['pending_blockers']
      message: string
    }>(`${API_BASE}/traders/${traderId}/stop`)
    if (!result.success) throw new Error(result.message || '停止交易员失败')
    return result.data!
  },

  async reconcileTrader(traderId: string): Promise<{
    status: string
    lifecycle_generation: number
    pending_blockers: TraderInfo['pending_blockers']
    message: string
  }> {
    const result = await httpClient.post<{
      status: string
      lifecycle_generation: number
      pending_blockers: TraderInfo['pending_blockers']
      message: string
    }>(`${API_BASE}/traders/${traderId}/reconcile`)
    if (!result.success) {
      throw new Error(result.message || '交易员权威对账失败')
    }
    return result.data!
  },

  async toggleCompetition(
    traderId: string,
    showInCompetition: boolean
  ): Promise<void> {
    const result = await httpClient.put(
      `${API_BASE}/traders/${traderId}/competition`,
      { show_in_competition: showInCompetition }
    )
    if (!result.success) throw new Error('更新竞技场显示设置失败')
  },

  async closePosition(
    traderId: string,
    symbol: string,
    side: string
  ): Promise<{ message: string }> {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/traders/${traderId}/close-position`,
      { symbol, side }
    )
    if (!result.success) throw new Error('平仓失败')
    return result.data!
  },

  async updateTraderPrompt(
    traderId: string,
    customPrompt: string
  ): Promise<void> {
    const result = await httpClient.put(
      `${API_BASE}/traders/${traderId}/prompt`,
      { custom_prompt: customPrompt }
    )
    if (!result.success) throw new Error('更新自定义策略失败')
  },

  async getTraderConfig(traderId: string): Promise<TraderConfigData> {
    const result = await httpClient.get<TraderConfigData>(
      `${API_BASE}/traders/${traderId}/config`
    )
    if (!result.success) throw new Error('获取交易员配置失败')
    return result.data!
  },

  async updateTrader(
    traderId: string,
    request: CreateTraderRequest
  ): Promise<TraderInfo> {
    const result = await httpClient.put<TraderInfo>(
      `${API_BASE}/traders/${traderId}`,
      request
    )
    if (!result.success) throw new Error('更新交易员失败')
    return result.data!
  },

  // AI模型配置接口
  async getModelConfigs(): Promise<AIModel[]> {
    const result = await httpClient.get<AIModel[]>(`${API_BASE}/models`)
    if (!result.success) throw new Error('获取模型配置失败')
    return Array.isArray(result.data) ? result.data : []
  },

  // 获取系统支持的AI模型列表（无需认证）
  async getSupportedModels(): Promise<AIModel[]> {
    const result = await httpClient.get<AIModel[]>(
      `${API_BASE}/supported-models`
    )
    if (!result.success) throw new Error('获取支持的模型失败')
    return result.data!
  },

  async getPromptTemplates(): Promise<string[]> {
    const res = await fetch(`${API_BASE}/prompt-templates`)
    if (!res.ok) throw new Error('获取提示词模板失败')
    const data = await res.json()
    if (Array.isArray(data.templates)) {
      return data.templates.map((item: { name: string }) => item.name)
    }
    return []
  },

  async updateModelConfigs(request: UpdateModelConfigRequest): Promise<void> {
    // 检查是否启用了传输加密
    const config = await CryptoService.fetchCryptoConfig()

    if (!config.transport_encryption) {
      // 传输加密禁用时，直接发送明文
      const result = await httpClient.put(`${API_BASE}/models`, request)
      if (!result.success) throw new Error('更新模型配置失败')
      return
    }

    // 获取RSA公钥
    const publicKey = await CryptoService.fetchPublicKey()

    // 初始化加密服务
    await CryptoService.initialize(publicKey)

    // 获取用户信息（从localStorage或其他地方）
    const userId = localStorage.getItem('user_id') || ''
    const sessionId = sessionStorage.getItem('session_id') || ''

    // 加密敏感数据
    const encryptedPayload = await CryptoService.encryptSensitiveData(
      JSON.stringify(request),
      userId,
      sessionId
    )

    // 发送加密数据
    const result = await httpClient.put(`${API_BASE}/models`, encryptedPayload)
    if (!result.success) throw new Error('更新模型配置失败')
  },

  // 交易所配置接口
  async getExchangeConfigs(): Promise<Exchange[]> {
    const result = await httpClient.get<Exchange[]>(`${API_BASE}/exchanges`)
    if (!result.success) throw new Error('获取交易所配置失败')
    return result.data!
  },

  // 获取系统支持的交易所列表（无需认证）
  async getSupportedExchanges(): Promise<Exchange[]> {
    const result = await httpClient.get<Exchange[]>(
      `${API_BASE}/supported-exchanges`
    )
    if (!result.success) throw new Error('获取支持的交易所失败')
    return result.data!
  },

  async updateExchangeConfigs(
    request: UpdateExchangeConfigRequest
  ): Promise<void> {
    const result = await httpClient.put(`${API_BASE}/exchanges`, request)
    if (!result.success) throw new Error('更新交易所配置失败')
  },

  async getCopyGuardAccountRiskPolicy(
    exchangeId: string
  ): Promise<CopyGuardAccountRiskPolicyResponse> {
    const result = await httpClient.get<CopyGuardAccountRiskPolicyResponse>(
      `${API_BASE}/copytrade/risk/accounts/${exchangeId}/policy`
    )
    if (!result.success || !result.data) throw new Error('获取账户保护配置失败')
    return result.data
  },

  async updateCopyGuardAccountRiskPolicy(
    exchangeId: string,
    maxPositionLossPct: number,
    highRiskConfirmed = false,
    extremeConfirmValue?: number
  ): Promise<void> {
    const result = await httpClient.put(
      `${API_BASE}/copytrade/risk/accounts/${exchangeId}/policy`,
      {
        copy_guard_max_position_loss_pct: maxPositionLossPct,
        risk_high_risk_confirmed: highRiskConfirmed,
        risk_extreme_risk_confirm_value: extremeConfirmValue,
      }
    )
    if (!result.success) throw new Error('保存账户保护配置失败')
  },

  // 创建新的交易所账户
  async createExchange(
    request: CreateExchangeRequest
  ): Promise<{ id: string }> {
    const result = await httpClient.post<{ id: string }>(
      `${API_BASE}/exchanges`,
      request
    )
    if (!result.success) throw new Error('创建交易所账户失败')
    return result.data!
  },

  // 创建新的交易所账户（加密传输）
  async createExchangeEncrypted(
    request: CreateExchangeRequest
  ): Promise<{ id: string }> {
    // 检查是否启用了传输加密
    const config = await CryptoService.fetchCryptoConfig()

    if (!config.transport_encryption) {
      // 传输加密禁用时，直接发送明文
      const result = await httpClient.post<{ id: string }>(
        `${API_BASE}/exchanges`,
        request
      )
      if (!result.success) throw new Error('创建交易所账户失败')
      return result.data!
    }

    // 获取RSA公钥
    const publicKey = await CryptoService.fetchPublicKey()

    // 初始化加密服务
    await CryptoService.initialize(publicKey)

    // 获取用户信息
    const userId = localStorage.getItem('user_id') || ''
    const sessionId = sessionStorage.getItem('session_id') || ''

    // 加密敏感数据
    const encryptedPayload = await CryptoService.encryptSensitiveData(
      JSON.stringify(request),
      userId,
      sessionId
    )

    // 发送加密数据
    const result = await httpClient.post<{ id: string }>(
      `${API_BASE}/exchanges`,
      encryptedPayload
    )
    if (!result.success) throw new Error('创建交易所账户失败')
    return result.data!
  },

  // 删除交易所账户
  async deleteExchange(exchangeId: string): Promise<void> {
    const result = await httpClient.delete(
      `${API_BASE}/exchanges/${exchangeId}`
    )
    if (!result.success) throw new Error('删除交易所账户失败')
  },

  // 使用加密传输更新交易所配置（自动检测是否启用加密）
  async updateExchangeConfigsEncrypted(
    request: UpdateExchangeConfigRequest
  ): Promise<void> {
    // 检查是否启用了传输加密
    const config = await CryptoService.fetchCryptoConfig()

    if (!config.transport_encryption) {
      // 传输加密禁用时，直接发送明文
      const result = await httpClient.put(`${API_BASE}/exchanges`, request)
      if (!result.success) throw new Error('更新交易所配置失败')
      return
    }

    // 获取RSA公钥
    const publicKey = await CryptoService.fetchPublicKey()

    // 初始化加密服务
    await CryptoService.initialize(publicKey)

    // 获取用户信息（从localStorage或其他地方）
    const userId = localStorage.getItem('user_id') || ''
    const sessionId = sessionStorage.getItem('session_id') || ''

    // 加密敏感数据
    const encryptedPayload = await CryptoService.encryptSensitiveData(
      JSON.stringify(request),
      userId,
      sessionId
    )

    // 发送加密数据
    const result = await httpClient.put(
      `${API_BASE}/exchanges`,
      encryptedPayload
    )
    if (!result.success) throw new Error('更新交易所配置失败')
  },

  // 获取系统状态（支持trader_id）
  async getStatus(traderId?: string): Promise<SystemStatus> {
    const url = traderId
      ? `${API_BASE}/status?trader_id=${traderId}`
      : `${API_BASE}/status`
    const result = await httpClient.get<SystemStatus>(url)
    if (!result.success) throw new Error('获取系统状态失败')
    return result.data!
  },

  // 获取账户信息（支持trader_id）
  async getAccount(traderId?: string): Promise<AccountInfo> {
    const url = traderId
      ? `${API_BASE}/account?trader_id=${traderId}`
      : `${API_BASE}/account`
    const result = await httpClient.get<AccountInfo>(url)
    if (!result.success) throw new Error('获取账户信息失败')
    console.log('Account data fetched:', result.data)
    return result.data!
  },

  // 获取持仓列表（支持trader_id）
  async getPositions(traderId?: string): Promise<Position[]> {
    const url = traderId
      ? `${API_BASE}/positions?trader_id=${traderId}`
      : `${API_BASE}/positions`
    const result = await httpClient.get<Position[]>(url)
    if (!result.success) throw new Error('获取持仓列表失败')
    return result.data!
  },

  // 获取决策日志（支持trader_id）
  async getDecisions(traderId?: string): Promise<DecisionRecord[]> {
    const url = traderId
      ? `${API_BASE}/decisions?trader_id=${traderId}`
      : `${API_BASE}/decisions`
    const result = await httpClient.get<DecisionRecord[]>(url)
    if (!result.success) throw new Error('获取决策日志失败')
    return result.data!
  },

  // 获取最新决策（支持trader_id和limit参数）
  async getLatestDecisions(
    traderId?: string,
    limit: number = 5
  ): Promise<DecisionRecord[]> {
    const params = new URLSearchParams()
    if (traderId) {
      params.append('trader_id', traderId)
    }
    params.append('limit', limit.toString())

    const result = await httpClient.get<DecisionRecord[]>(
      `${API_BASE}/decisions/latest?${params}`
    )
    if (!result.success) throw new Error('获取最新决策失败')
    return result.data!
  },

  // 获取统计信息（支持trader_id）
  async getStatistics(traderId?: string): Promise<Statistics> {
    const url = traderId
      ? `${API_BASE}/statistics?trader_id=${traderId}`
      : `${API_BASE}/statistics`
    const result = await httpClient.get<Statistics>(url)
    if (!result.success) throw new Error('获取统计信息失败')
    return result.data!
  },

  // 获取收益率历史数据（支持trader_id）
  async getEquityHistory(traderId?: string): Promise<any[]> {
    const url = traderId
      ? `${API_BASE}/equity-history?trader_id=${traderId}`
      : `${API_BASE}/equity-history`
    const result = await httpClient.get<any[]>(url)
    if (!result.success) throw new Error('获取历史数据失败')
    return result.data!
  },

  // 批量获取多个交易员的历史数据（无需认证）
  async getEquityHistoryBatch(traderIds: string[]): Promise<any> {
    const result = await httpClient.request<any>(
      `${API_BASE}/equity-history-batch`,
      { method: 'POST', data: { trader_ids: traderIds }, timeout: 15000 }
    )
    if (!result.success) throw new Error('获取批量历史数据失败')
    const data = result.data!
    const histories = data?.histories || {}
    const errors = data?.errors || {}
    if (
      traderIds.length > 0 &&
      traderIds.every((traderId) => errors[traderId] && !histories[traderId])
    ) {
      throw new Error('历史数据服务暂时不可用')
    }
    return data
  },

  // 获取前5名交易员数据（无需认证）
  async getTopTraders(): Promise<any[]> {
    const result = await httpClient.get<any[]>(`${API_BASE}/top-traders`)
    if (!result.success) throw new Error('获取前5名交易员失败')
    return result.data!
  },

  // 获取公开交易员配置（无需认证）
  async getPublicTraderConfig(traderId: string): Promise<any> {
    const result = await httpClient.get<any>(
      `${API_BASE}/trader/${traderId}/config`
    )
    if (!result.success) throw new Error('获取公开交易员配置失败')
    return result.data!
  },

  // 获取竞赛数据（无需认证）
  async getCompetition(): Promise<CompetitionData> {
    const result = await httpClient.get<CompetitionData>(
      `${API_BASE}/competition`
    )
    if (!result.success) throw new Error('获取竞赛数据失败')
    return result.data!
  },

  // 获取服务器IP（需要认证，用于白名单配置）
  async getServerIP(): Promise<{
    public_ip: string
    message: string
  }> {
    const result = await httpClient.get<{
      public_ip: string
      message: string
    }>(`${API_BASE}/server-ip`)
    if (!result.success) throw new Error('获取服务器IP失败')
    return result.data!
  },

  // Backtest APIs
  async getBacktestRuns(params?: {
    state?: string
    search?: string
    limit?: number
    offset?: number
  }): Promise<BacktestRunsResponse> {
    const query = new URLSearchParams()
    if (params?.state) query.set('state', params.state)
    if (params?.search) query.set('search', params.search)
    if (params?.limit) query.set('limit', String(params.limit))
    if (params?.offset) query.set('offset', String(params.offset))
    const res = await fetch(
      `${API_BASE}/backtest/runs${query.toString() ? `?${query}` : ''}`,
      {
        headers: getAuthHeaders(),
      }
    )
    return handleJSONResponse<BacktestRunsResponse>(res)
  },

  async startBacktest(
    config: BacktestStartConfig
  ): Promise<BacktestRunMetadata> {
    const res = await fetch(`${API_BASE}/backtest/start`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ config }),
    })
    return handleJSONResponse<BacktestRunMetadata>(res)
  },

  async pauseBacktest(runId: string): Promise<BacktestRunMetadata> {
    const res = await fetch(`${API_BASE}/backtest/pause`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ run_id: runId }),
    })
    return handleJSONResponse<BacktestRunMetadata>(res)
  },

  async resumeBacktest(runId: string): Promise<BacktestRunMetadata> {
    const res = await fetch(`${API_BASE}/backtest/resume`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ run_id: runId }),
    })
    return handleJSONResponse<BacktestRunMetadata>(res)
  },

  async stopBacktest(runId: string): Promise<BacktestRunMetadata> {
    const res = await fetch(`${API_BASE}/backtest/stop`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ run_id: runId }),
    })
    return handleJSONResponse<BacktestRunMetadata>(res)
  },

  async updateBacktestLabel(
    runId: string,
    label: string
  ): Promise<BacktestRunMetadata> {
    const res = await fetch(`${API_BASE}/backtest/label`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ run_id: runId, label }),
    })
    return handleJSONResponse<BacktestRunMetadata>(res)
  },

  async deleteBacktestRun(runId: string): Promise<void> {
    const res = await fetch(`${API_BASE}/backtest/delete`, {
      method: 'POST',
      headers: getAuthHeaders(),
      body: JSON.stringify({ run_id: runId }),
    })
    if (!res.ok) {
      throw new Error(await res.text())
    }
  },

  async getBacktestStatus(runId: string): Promise<BacktestStatusPayload> {
    const res = await fetch(`${API_BASE}/backtest/status?run_id=${runId}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<BacktestStatusPayload>(res)
  },

  async getBacktestEquity(
    runId: string,
    timeframe?: string,
    limit?: number
  ): Promise<BacktestEquityPoint[]> {
    const query = new URLSearchParams({ run_id: runId })
    if (timeframe) query.set('tf', timeframe)
    if (limit) query.set('limit', String(limit))
    const res = await fetch(`${API_BASE}/backtest/equity?${query}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<BacktestEquityPoint[]>(res)
  },

  async getBacktestTrades(
    runId: string,
    limit = 200
  ): Promise<BacktestTradeEvent[]> {
    const query = new URLSearchParams({
      run_id: runId,
      limit: String(limit),
    })
    const res = await fetch(`${API_BASE}/backtest/trades?${query}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<BacktestTradeEvent[]>(res)
  },

  async getBacktestMetrics(runId: string): Promise<BacktestMetrics> {
    const res = await fetch(`${API_BASE}/backtest/metrics?run_id=${runId}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<BacktestMetrics>(res)
  },

  async getBacktestTrace(
    runId: string,
    cycle?: number
  ): Promise<DecisionRecord> {
    const query = new URLSearchParams({ run_id: runId })
    if (cycle) query.set('cycle', String(cycle))
    const res = await fetch(`${API_BASE}/backtest/trace?${query}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<DecisionRecord>(res)
  },

  async getBacktestDecisions(
    runId: string,
    limit = 20,
    offset = 0
  ): Promise<DecisionRecord[]> {
    const query = new URLSearchParams({
      run_id: runId,
      limit: String(limit),
      offset: String(offset),
    })
    const res = await fetch(`${API_BASE}/backtest/decisions?${query}`, {
      headers: getAuthHeaders(),
    })
    return handleJSONResponse<DecisionRecord[]>(res)
  },

  async exportBacktest(runId: string): Promise<Blob> {
    const res = await fetch(`${API_BASE}/backtest/export?run_id=${runId}`, {
      headers: getAuthHeaders(),
    })
    if (!res.ok) {
      const text = await res.text()
      try {
        const data = text ? JSON.parse(text) : null
        throw new Error(
          data?.error || data?.message || text || '导出失败，请稍后再试'
        )
      } catch (err) {
        if (err instanceof Error && err.message) {
          throw err
        }
        throw new Error(text || '导出失败，请稍后再试')
      }
    }
    return res.blob()
  },

  // Strategy APIs
  async getStrategies(): Promise<Strategy[]> {
    const result = await httpClient.get<{ strategies: Strategy[] }>(
      `${API_BASE}/strategies`
    )
    if (!result.success) throw new Error('获取策略列表失败')
    const strategies = result.data?.strategies
    return Array.isArray(strategies) ? strategies : []
  },

  async getStrategy(strategyId: string): Promise<Strategy> {
    const result = await httpClient.get<Strategy>(
      `${API_BASE}/strategies/${strategyId}`
    )
    if (!result.success) throw new Error('获取策略失败')
    return result.data!
  },

  async getActiveStrategy(): Promise<Strategy> {
    const result = await httpClient.get<Strategy>(
      `${API_BASE}/strategies/active`
    )
    if (!result.success) throw new Error('获取激活策略失败')
    return result.data!
  },

  async getDefaultStrategyConfig(): Promise<StrategyConfig> {
    const result = await httpClient.get<StrategyConfig>(
      `${API_BASE}/strategies/default-config`
    )
    if (!result.success) throw new Error('获取默认策略配置失败')
    return result.data!
  },

  async createStrategy(data: {
    name: string
    description: string
    config: StrategyConfig
  }): Promise<Strategy> {
    const result = await httpClient.post<Strategy>(
      `${API_BASE}/strategies`,
      data
    )
    if (!result.success) throw new Error('创建策略失败')
    return result.data!
  },

  async updateStrategy(
    strategyId: string,
    data: {
      name?: string
      description?: string
      config?: StrategyConfig
    }
  ): Promise<Strategy> {
    const result = await httpClient.put<Strategy>(
      `${API_BASE}/strategies/${strategyId}`,
      data
    )
    if (!result.success) throw new Error('更新策略失败')
    return result.data!
  },

  async deleteStrategy(strategyId: string): Promise<void> {
    const result = await httpClient.delete(
      `${API_BASE}/strategies/${strategyId}`
    )
    if (!result.success) throw new Error('删除策略失败')
  },

  async activateStrategy(strategyId: string): Promise<Strategy> {
    const result = await httpClient.post<Strategy>(
      `${API_BASE}/strategies/${strategyId}/activate`
    )
    if (!result.success) throw new Error('激活策略失败')
    return result.data!
  },

  async duplicateStrategy(strategyId: string): Promise<Strategy> {
    const result = await httpClient.post<Strategy>(
      `${API_BASE}/strategies/${strategyId}/duplicate`
    )
    if (!result.success) throw new Error('复制策略失败')
    return result.data!
  },

  // Debate Arena APIs
  async getDebates(): Promise<DebateSession[]> {
    const result = await httpClient.get<DebateSession[]>(`${API_BASE}/debates`)
    if (!result.success) throw new Error('获取辩论列表失败')
    return Array.isArray(result.data) ? result.data : []
  },

  async getDebate(debateId: string): Promise<DebateSessionWithDetails> {
    const result = await httpClient.get<DebateSessionWithDetails>(
      `${API_BASE}/debates/${debateId}`
    )
    if (!result.success) throw new Error('获取辩论详情失败')
    return result.data!
  },

  async createDebate(
    request: CreateDebateRequest
  ): Promise<DebateSessionWithDetails> {
    const result = await httpClient.post<DebateSessionWithDetails>(
      `${API_BASE}/debates`,
      request
    )
    if (!result.success) throw new Error('创建辩论失败')
    return result.data!
  },

  async startDebate(debateId: string): Promise<void> {
    const result = await httpClient.post(
      `${API_BASE}/debates/${debateId}/start`
    )
    if (!result.success) throw new Error('启动辩论失败')
  },

  async cancelDebate(debateId: string): Promise<void> {
    const result = await httpClient.post(
      `${API_BASE}/debates/${debateId}/cancel`
    )
    if (!result.success) throw new Error('取消辩论失败')
  },

  async executeDebate(
    debateId: string,
    traderId: string
  ): Promise<DebateSessionWithDetails> {
    const result = await httpClient.post<{
      message: string
      session: DebateSessionWithDetails
    }>(`${API_BASE}/debates/${debateId}/execute`, { trader_id: traderId })
    if (!result.success) throw new Error('执行交易失败')
    return result.data!.session
  },

  async deleteDebate(debateId: string): Promise<void> {
    const result = await httpClient.delete(`${API_BASE}/debates/${debateId}`)
    if (!result.success) throw new Error('删除辩论失败')
  },

  async getDebateMessages(debateId: string): Promise<DebateMessage[]> {
    const result = await httpClient.get<DebateMessage[]>(
      `${API_BASE}/debates/${debateId}/messages`
    )
    if (!result.success) throw new Error('获取辩论消息失败')
    return result.data!
  },

  async getDebateVotes(debateId: string): Promise<DebateVote[]> {
    const result = await httpClient.get<DebateVote[]>(
      `${API_BASE}/debates/${debateId}/votes`
    )
    if (!result.success) throw new Error('获取辩论投票失败')
    return result.data!
  },

  async getDebatePersonalities(): Promise<DebatePersonalityInfo[]> {
    const result = await httpClient.get<DebatePersonalityInfo[]>(
      `${API_BASE}/debates/personalities`
    )
    if (!result.success) throw new Error('获取AI性格列表失败')
    return result.data!
  },

  // SSE stream for live debate updates
  createDebateStream(debateId: string): EventSource {
    const token = localStorage.getItem('auth_token')
    return new EventSource(
      `${API_BASE}/debates/${debateId}/stream?token=${token}`
    )
  },

  // ==========================================================================
  // Binance 全局共享凭证管理（v2 凭证全局化）
  // ==========================================================================
  // 所有 Binance 跟单 trader 共享同一份凭证；一处更新全局生效，无需逐个 trader 维护

  async listBinanceCredentials(): Promise<BinanceCredentialsView[]> {
    const result = await httpClient.get<BinanceCredentialsListResponse>(
      `${API_BASE}/copytrade/binance-credentials`
    )
    if (!result.success)
      throw new Error(result.message || '获取 Binance 凭证失败')
    return result.data?.credentials ?? []
  },

  async setBinanceCredentials(
    req: BinanceCredentialsSetRequest
  ): Promise<BinanceCredentialsView | null> {
    const result = await httpClient.post<{
      message: string
      credentials: BinanceCredentialsView
    }>(`${API_BASE}/copytrade/binance-credentials`, req)
    if (!result.success)
      throw new Error(result.message || '保存 Binance 凭证失败')
    return result.data?.credentials ?? null
  },

  async testBinanceCredentials(
    label?: string
  ): Promise<BinanceCredentialsTestResponse> {
    const url = label
      ? `${API_BASE}/copytrade/binance-credentials/test?label=${encodeURIComponent(label)}`
      : `${API_BASE}/copytrade/binance-credentials/test`
    const result = await httpClient.post<BinanceCredentialsTestResponse>(url)
    if (!result.success)
      throw new Error(result.message || '测试 Binance 凭证失败')
    return result.data!
  },

  async deleteBinanceCredentials(label: string): Promise<void> {
    const result = await httpClient.delete(
      `${API_BASE}/copytrade/binance-credentials/${encodeURIComponent(label)}`
    )
    if (!result.success)
      throw new Error(result.message || '删除 Binance 凭证失败')
  },

  async getBinanceCredentialsAffectedTraders(): Promise<string[]> {
    const result = await httpClient.get<BinanceCredentialsAffectedResponse>(
      `${API_BASE}/copytrade/binance-credentials/affected`
    )
    if (!result.success)
      throw new Error(result.message || '获取受影响交易员失败')
    return result.data?.trader_ids ?? []
  },

  async getCopyGuardSummary(params = '') {
    const result = await httpClient.get<{
      summary: import('../types').CopyGuardSummary
      shadow_promotion: import('../types').CopyGuardShadowPromotionReport
    }>(`${API_BASE}/copytrade/risk/summary${params}`)
    if (!result.success)
      throw new Error(result.message || '获取 Copy Guard 统计失败')
    return {
      ...result.data!.summary,
      shadow_promotion: result.data!.shadow_promotion,
    }
  },

  async getCopyGuardCycles(params = '') {
    const result = await httpClient.get<{
      cycles: import('../types').CopyGuardCycle[]
    }>(`${API_BASE}/copytrade/risk/cycles${params}`)
    if (!result.success)
      throw new Error(result.message || '获取 Copy Guard 明细失败')
    return result.data!.cycles
  },
  async getCopyGuardCycle(id: number) {
    const result = await httpClient.get<{
      cycle: import('../types').CopyGuardCycle
      execution_intents: import('../types').CopyTradeExecutionIntent[]
      events: Array<{
        id: number
        type: string
        price: number
        quantity: number
        pnl: number
        fee: number
        metadata?: Record<string, unknown>
        created_at: string
      }>
      attempts: Array<{
        id: number
        attempt_no: number
        status: string
        entry_price: number
        exit_price: number
        quantity: number
        notional: number
        pnl: number
        fee: number
        funding_fee: number
        atr: number
        actual_leverage: number
        initial_margin_basis: number
        planned_notional: number
        promoted_notional: number
        promotion_reason?: string
        ai_stop_price: number
        final_stop_price: number
        stop_trigger_price: number
        stop_anchor_entry_price: number
        stop_anchor_leverage: number
        stop_anchor_initial_margin: number
        stop_anchor_price: number
        stop_configured_margin_loss_pct: number
        stop_validation_result?: string
        expected_position_loss_pct: number
        actual_position_loss_pct: number
        governed_by?: string
        follower_pos_id: string
        entry_order_id: string
        exit_order_id: string
        opened_at: string
        closed_at?: string
      }>
      protection?: {
        algo_id: string
        algo_client_id: string
        quantity: number
        trigger_price: number
        trigger_type: string
        status: string
      }
      // v4.1 观察期采样时间线（出局后每个 tick 的价格/边界/门控轨迹）
      watch_samples?: Array<{
        id: number
        cycle_id: number
        attempt_no: number
        mark_price: number
        atr: number
        leader_entry_price: number
        leader_size: number
        reentry_boundary: number
        chase_limit: number
        gate: string
        created_at: string
      }>
      ai_candidates: import('../types').CopyGuardAICandidate[]
      ai_analyses: import('../types').ReentryAIAnalysis[]
      ai_decision_evaluations: import('../types').ReentryAIDecisionEvaluation[]
      ai_effect_summary: import('../types').CopyGuardAIEffectSummary
      shadow_evaluations: import('../types').CopyGuardShadowEvaluation[]
      attribution: {
        final: boolean
        leader_direction_return: number
        baseline_no_guard_pnl: number
        stop_only_pnl: number
        actual_copy_guard_pnl: number
        stop_savings: number
        missed_profit: number
        reentry_contribution: number
        first_reentry_pnl: number
        second_reentry_pnl: number
        fees: number
        slippage: number
        realized_path_max_drawdown_usd: number
        worst_attempt_pnl: number
        max_post_stop_mfe_usd: number
        max_post_stop_mae_usd: number
        attempts: Array<{
          attempt_no: number
          pnl: number
          fee: number
          funding_fee: number
          stop_only_path: boolean
          first_recovery_seconds?: number
          post_stop_mfe_usd?: number
          post_stop_mae_usd?: number
        }>
      }
      schema_version: number
      defaults_version: number
    }>(`${API_BASE}/copytrade/risk/cycles/${id}`)
    if (!result.success)
      throw new Error(result.message || '获取 Copy Guard 生命周期失败')
    return result.data!
  },

  async getCopyEvents(params = '') {
    const result = await httpClient.get<{
      events: import('../types').CopyTradeEvent[]
      count: number
      total: number
      traders?: Record<string, string> // 全部交易员 id->名称，供筛选下拉
    }>(`${API_BASE}/copytrade/events${params}`)
    if (!result.success)
      throw new Error(result.message || '获取跟单事件日志失败')
    return {
      events: result.data!.events ?? [],
      total: result.data!.total ?? 0,
      traders: result.data!.traders ?? {},
    }
  },

  // Copy Guard v7 持久化 AI 重入候选
  async getCopyGuardAICandidates(params = '') {
    const result = await httpClient.get<{
      candidates: import('../types').CopyGuardAICandidate[]
      trader_names: Record<string, string>
    }>(`${API_BASE}/copytrade/risk/ai-candidates${params}`)
    if (!result.success)
      throw new Error(result.message || '获取 AI 重入候选失败')
    return {
      candidates: result.data!.candidates ?? [],
      traderNames: result.data!.trader_names ?? {},
    }
  },
  async pauseCopyGuardAICandidate(id: number) {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/copytrade/risk/ai-candidates/${id}/pause`
    )
    if (!result.success) throw new Error(result.message || '暂停候选失败')
  },
  async resumeCopyGuardAICandidate(id: number) {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/copytrade/risk/ai-candidates/${id}/resume`
    )
    if (!result.success) throw new Error(result.message || '恢复候选失败')
  },
  async requestCopyGuardAICandidateReview(id: number) {
    const result = await httpClient.post<{
      message: string
      candidate: import('../types').CopyGuardAICandidate
      eligible_at: string
      may_execute_real_order: boolean
    }>(`${API_BASE}/copytrade/risk/ai-candidates/${id}/request-review`)
    if (!result.success) throw new Error(result.message || '请求 AI 复查失败')
    return result.data!
  },
  async terminateCopyGuardAICandidate(id: number) {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/copytrade/risk/ai-candidates/${id}/terminate`
    )
    if (!result.success) throw new Error(result.message || '终止候选失败')
  },

  // 重入 AI 助手（Reentry Advisor 插件）
  async getReentryAnalyses(signalId: number) {
    const result = await httpClient.get<{
      analyses: import('../types').ReentryAIAnalysis[]
    }>(`${API_BASE}/reentry-advisor/signals/${signalId}/analyses`)
    if (!result.success)
      throw new Error(result.message || '获取重入分析数据失败')
    return result.data!.analyses
  },
  async regenerateReentryAnalysis(signalId: number) {
    const result = await httpClient.post<{
      message: string
      analysis: import('../types').ReentryAIAnalysis
    }>(`${API_BASE}/reentry-advisor/signals/${signalId}/regenerate`)
    if (!result.success)
      throw new Error(result.message || '重新生成分析数据失败')
    return result.data!
  },
  async saveReentryExternal(
    analysisId: number,
    body: { external_response: string; external_verdict: string }
  ) {
    const result = await httpClient.put<{
      message: string
      analysis: import('../types').ReentryAIAnalysis
    }>(`${API_BASE}/reentry-advisor/analyses/${analysisId}/external`, body)
    if (!result.success)
      throw new Error(result.message || '保存外部 AI 结论失败')
    return result.data!
  },
  // Phase 2：手动触发内置 AI 分析（异步，结果稍后写回记录）
  async analyzeReentryAnalysis(analysisId: number) {
    const result = await httpClient.post<{ message: string }>(
      `${API_BASE}/reentry-advisor/analyses/${analysisId}/analyze`
    )
    if (!result.success) throw new Error(result.message || '触发 AI 分析失败')
    return result.data!
  },
  async getReentryAnalysis(analysisId: number) {
    const result = await httpClient.get<{
      analysis: import('../types').ReentryAIAnalysis
      evaluations: import('../types').ReentryAIDecisionEvaluation[]
    }>(`${API_BASE}/reentry-advisor/analyses/${analysisId}`)
    if (!result.success)
      throw new Error(result.message || '获取 AI 分析详情失败')
    return result.data!
  },
  async getReentryConfig() {
    const result = await httpClient.get<{
      config: import('../types').ReentryAIConfig
      production_prompt: string
      production_prompt_version: string
      legacy_default_prompt: string
      confidence_source: string
      recommended_confidence: number
    }>(`${API_BASE}/reentry-advisor/config`)
    if (!result.success)
      throw new Error(result.message || '获取重入 AI 配置失败')
    return result.data!
  },
  async saveReentryConfig(config: import('../types').ReentryAIConfig) {
    const result = await httpClient.put<{
      message: string
      config: import('../types').ReentryAIConfig
    }>(`${API_BASE}/reentry-advisor/config`, config)
    if (!result.success)
      throw new Error(result.message || '保存重入 AI 配置失败')
    return result.data!
  },
  async testReentryAIConnection() {
    const result = await httpClient.post<{
      diagnostic: import('../types').ReentryAIDiagnostic
    }>(`${API_BASE}/reentry-advisor/connection-test`)
    if (!result.success) throw new Error(result.message || 'AI 连接自检失败')
    return result.data!.diagnostic
  },
  async getReentryAIDiagnostics(limit = 10) {
    const result = await httpClient.get<{
      diagnostics: import('../types').ReentryAIDiagnostic[]
    }>(`${API_BASE}/reentry-advisor/diagnostics?limit=${limit}`)
    if (!result.success)
      throw new Error(result.message || '获取 AI 自检记录失败')
    return result.data!.diagnostics
  },
  // 分析历史列表（跨信号，最新在前）
  async getReentryHistory(limit = 50) {
    const result = await httpClient.get<{
      analyses: import('../types').ReentryAIAnalysis[]
    }>(`${API_BASE}/reentry-advisor/analyses?limit=${limit}`)
    if (!result.success)
      throw new Error(result.message || '获取重入分析历史失败')
    return result.data!.analyses
  },
  // 市场指标实时预览（60s 后端缓存，与信号无关）
  async getReentryMarketPreview(symbol: string) {
    const result = await httpClient.get<{
      preview: import('../types').ReentryMarketPreview
    }>(
      `${API_BASE}/reentry-advisor/market-preview?symbol=${encodeURIComponent(symbol)}`
    )
    if (!result.success)
      throw new Error(result.message || '获取市场指标预览失败')
    return result.data!.preview
  },
  async getReentryStats() {
    const result = await httpClient.get<{
      stats: import('../types').ReentryAIStats
    }>(`${API_BASE}/reentry-advisor/stats`)
    if (!result.success)
      throw new Error(result.message || '获取重入 AI 统计失败')
    return result.data!.stats
  },
}
