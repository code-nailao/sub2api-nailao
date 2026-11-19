export type Benchmark = 'candy' | 'pelican' | 'creative'
export type RunStatus = 'queued' | 'submitting' | 'running' | 'succeeded' | 'failed' | 'unknown'
export interface MonitorPlan {
  visibility: 'admin' | 'authenticated'
  id: string
  name: string
  model: string
  reasoning_effort: string
  enabled: boolean
  candy_interval_seconds: number
  pelican_interval_seconds: number
  creative_every_five?: boolean
  next_drawing_benchmark?: 'pelican' | 'creative'
  next_candy_at: string
  next_pelican_at: string
}
export interface AdminMonitorPlan extends MonitorPlan {
  group_id: number
  key_status: string
  revision: number
  key_id: number
  owner_id: number
  daily_limit: number
  quota: number
  expires_at?: string
}
export interface MonitorRun {
  id: string
  plan_id: string
  name: string
  benchmark: Benchmark
  model: string
  reasoning_effort: string
  status: RunStatus
  phase: string
  verdict: string
  started_at: string
  finished_at?: string
  duration_ms?: number
  input_tokens?: number
  output_tokens?: number
  has_result: boolean
  has_html: boolean
  error_code?: string
  trigger: string
}
export interface MonitorData {
  plans: MonitorPlan[]
  runs: MonitorRun[]
  viewer_access: 'admin' | 'authenticated'
  admin_only: boolean
  can_manage: boolean
  server_time: string
}
export interface MonitorSettings {
  revision: number
  viewer_access: 'admin' | 'authenticated'
  admin_only: boolean
  plans: AdminMonitorPlan[]
  groups: Array<{ id: number; name: string; is_exclusive?: boolean }>
}
export interface MonitorResult { run: MonitorRun; answer: string; html: string }
export interface MonitorOrder {
  revision: number
  plans: Array<Pick<MonitorPlan, 'id' | 'name'>>
}
