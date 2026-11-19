let session: typeof import('../../internal/portal/web/session.js') | undefined

const messages: Record<string, string> = {
  admin_login_required: '请先登录主站后查看检测记录。',
  admin_required: '此页面暂未向普通用户开放。',
  main_identity_unavailable: '登录状态暂时无法核验，请稍后重试。',
  settings_changed: '配置已更新，请重新载入后保存。',
  monitor_key_unavailable: '检测密钥不可用，请检查所属分组、额度和有效期。',
  test_in_progress: '该项检测正在进行，请等待现有任务完成。',
  key_creation_needs_review: '密钥创建结果待核对，请重新载入配置，不要重复创建。',
  record_unavailable: '这条记录已过期或结果尚未就绪。',
  invalid_settings: '请检查检测间隔、模型和额度设置。',
}

export class MonitorError extends Error {
  constructor(public status: number, public code: string) {
    super(messages[code] || '操作暂未完成，请稍后重试。')
  }
}

async function sessionModule() {
  try {
    session ??= await import('../../internal/portal/web/session.js')
    await session.currentToken()
    return session
  } catch {
    throw new MonitorError(401, 'admin_login_required')
  }
}

async function authenticatedRequest(path: string, options: { method?: string; body?: unknown } = {}) {
  const auth = await sessionModule()
  try {
    return await auth.authorizedJSON(path, options)
  } catch {
    try {
      if (!auth.isCurrentSession()) throw new Error('logged out')
    } catch { throw new MonitorError(401, 'admin_login_required') }
    throw new MonitorError(503, 'operation_failed')
  }
}

export async function monitorRequest<T>(suffix: string, method = 'GET', body?: unknown): Promise<T> {
  if (!/^\/(?:settings|order|plans(?:\/[a-f0-9]{32}(?:\/(?:run|clear-history))?)?)$/.test(suffix)) throw new Error('无效的检测操作')
  const result = await authenticatedRequest('/api/intelligence-tests' + suffix, { method, body })
  if (!result.ok) throw new MonitorError(result.status, result.data?.error || 'operation_failed')
  return result.data as T
}

export async function monitorRead<T>(suffix = ''): Promise<T> {
  if (suffix !== '' && !/^\/runs\/[a-f0-9]{32}$/.test(suffix)) throw new Error('无效的检测记录')
  const result = await authenticatedRequest('/api/intelligence-tests' + (suffix || '/snapshot'))
  if (!result.ok) throw new MonitorError(result.status, result.data?.error || 'operation_failed')
  return result.data as T
}
