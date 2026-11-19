<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { VueDraggable, type DraggableEvent } from 'vue-draggable-plus'
import { palettes, type ThemeColor } from '../../../sources-brand/tokens.mjs'
import type { ColorMode } from '../../../sources-brand/appearance'
import { embedded, intelligenceAppearance } from './intelligence-appearance'
import { MonitorError, monitorRead, monitorRequest } from './intelligence-api'
import type { AdminMonitorPlan, Benchmark, MonitorData, MonitorOrder, MonitorPlan, MonitorResult, MonitorRun, MonitorSettings } from './intelligence-types'
import IntelligenceArtwork from './IntelligenceArtwork.vue'
import { createResultCache } from './intelligence-results'

const look = intelligenceAppearance()
const appearanceState = look.state
const appearanceOpen = ref(false), data = ref<MonitorData>(), error = ref(''), refreshing = ref(false)
const drawingFilter = ref('all')
const group = ref('all'), filter = ref('all'), notice = ref(''), now = ref(Date.now())
const settings = ref<MonitorSettings>(), settingsOpen = ref(false), settingsBusy = ref(false), formError = ref('')
const order = ref<MonitorOrder>(), orderOpen = ref(false), orderLoading = ref(false), orderSaving = ref(false), orderError = ref(''), orderNotice = ref('')
const orderDragging = ref(false)
const orderAnimation = matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : 200
const savedOrder = ref<string[]>([]), orderDialog = ref<HTMLDialogElement>()
const editing = ref<string>(), saving = ref(false), running = ref('')
const deleting = ref(''), deleteNotice = ref('')
const selected = ref<MonitorRun>(), result = ref<MonitorResult>(), resultError = ref(''), resultLoading = ref(false)
const view = ref<'fit' | 'actual'>('fit'), artRatio = ref(4 / 3)
const detail = ref<HTMLDialogElement>(), method = ref<HTMLDialogElement>(), settingsDialog = ref<HTMLDialogElement>()
const tooltip = ref<{ run: MonitorRun; x: number; y: number }>()
const form = ref({ group_id: 0, visibility: 'admin', name: '', reasoning_effort: 'medium', enabled: false, creative_every_five: false, candy_minutes: 5, pelican_minutes: 20, daily_limit: 20, quota: 200 })
const resultCache = createResultCache(id => monitorRead<MonitorResult>('/runs/'+id))
const pageVisible = ref(!document.hidden), methodOpen = ref(false), lastChecked = ref('')
const previewsSuspended = computed(() => !pageVisible.value || Boolean(selected.value) || settingsOpen.value || orderOpen.value || methodOpen.value)
let displaySignature = ''
const pendingManual = new Map<string,string>()
let epoch = 0, orderLoadSequence = 0, orderGeneration = 0, pollTimer: ReturnType<typeof setTimeout>, clockTimer: ReturnType<typeof setInterval>, loadBusy = false
const cards = computed(() => data.value?.plans || [])
const galleries = computed(() => cards.value.filter(p => group.value === 'all' || p.id === group.value))
const selectedPlan = computed(() => cards.value.find(p => p.id === selected.value?.plan_id))
const originalPlan = computed(() => settings.value?.plans.find(p => p.id === editing.value))
const orderChanged = computed(() => Boolean(order.value && order.value.plans.some((p, i) => p.id !== savedOrder.value[i])))
const selectedSiblings = computed(() => selected.value ? (data.value?.runs || []).filter(r => r.plan_id === selected.value?.plan_id && (selected.value?.benchmark==='candy'?r.benchmark==='candy':r.benchmark!=='candy'&&(drawingFilter.value==='all'||r.benchmark===drawingFilter.value))).sort((a,b) => b.started_at.localeCompare(a.started_at)) : [])
const selectedIndex = computed(() => selectedSiblings.value.findIndex(r => r.id === selected.value?.id))

function active(run: MonitorRun) { return ['queued', 'submitting', 'running'].includes(run.status) }
function candies(plan: MonitorPlan) { return (data.value?.runs || []).filter(r => r.plan_id === plan.id && r.benchmark === 'candy').slice(-60) }
function bars(plan: MonitorPlan): Array<MonitorRun | undefined> { const rows = candies(plan); return [...Array(Math.max(0,60-rows.length)), ...rows] }
function stats(plan: MonitorPlan) {
  const rows = candies(plan), correct = rows.filter(r => r.verdict === 'passed').length, wrong = rows.filter(r => r.verdict === 'incorrect').length
  return { pass: correct+wrong ? (correct/(correct+wrong)*100).toFixed(1) : '—', valid: correct+wrong, completed: rows.filter(r => !active(r)).length, pending: rows.filter(active).length, failed: rows.filter(r => !active(r) && !['passed','incorrect'].includes(r.verdict)).length, latest: rows.filter(r => !active(r)).at(-1), first: rows[0], last: rows.at(-1) }
}
function status(run?: MonitorRun) {
  if (!run) return 'empty'
  if (active(run)) return 'running'
  if (run.verdict === 'passed') return 'passed'
  if (run.verdict === 'incorrect') return 'incorrect'
  return 'error'
}
function verdict(run: MonitorRun) {
  if (run.status === 'queued') return '排队中'
  if (active(run)) return run.phase === 'classifying' ? '评估中' : '测试中'
  return ({ passed:'答对', incorrect:'答错', normal:'本次正常', degraded:'本次需关注', unknown:'失败 / 未知' } as Record<string,string>)[run.verdict] || '失败 / 未知'
}
function duration(run?: MonitorRun) {
  const ms = run?.duration_ms
  return ms === undefined ? { value:'—', unit:'' } : ms >= 1000 ? { value:(ms/1000).toFixed(1), unit:'秒' } : { value:String(ms), unit:'ms' }
}
function stamp(value?: string, short = false) {
  if (!value || value.startsWith('0001')) return '—'
  const d = new Date(value)
  return d.toLocaleString('zh-CN', { month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false, ...(short?{}:{year:'numeric'}) })
}
function ago(value?: string) {
  if (!value) return '暂无记录'
  const minutes = Math.max(0,Math.floor((now.value-new Date(value).getTime())/60000))
  return minutes < 1 ? '刚刚更新' : minutes < 60 ? '最近更新 '+minutes+' 分钟前' : '最近更新 '+Math.floor(minutes/60)+' 小时前'
}
function interval(seconds: number) { return seconds ? '每 '+(seconds/60)+' 分钟一次' : '未启用' }
function drawingLabel(kind?:string){return kind==='creative'?'自由发挥':'鹈鹕'}
function works(plan: MonitorPlan) {
  return (data.value?.runs || []).filter(r => r.plan_id === plan.id && r.benchmark !== 'candy')
    .sort((a,b) => b.started_at.localeCompare(a.started_at)).slice(0,20)
    .filter(r=>drawingFilter.value==='all'||r.benchmark===drawingFilter.value)
    .filter(r => filter.value === 'all' || (filter.value === 'normal' ? r.verdict === 'normal' : filter.value === 'unknown' ? r.verdict === 'unknown' && r.status === 'succeeded' : filter.value === 'degraded' ? r.verdict === 'degraded' : r.status === 'failed' || r.status === 'unknown'))
}
function showTooltip(event: MouseEvent | FocusEvent, run?: MonitorRun) {
  if (!run) return
  const rect = (event.currentTarget as HTMLElement).getBoundingClientRect()
  tooltip.value = { run, x:Math.max(12,Math.min(innerWidth-240,rect.left-90)), y:Math.max(8,rect.top-115) }
}
async function refresh(manual = false) {
  if (loadBusy) return
  loadBusy=true; refreshing.value=true
  const current=epoch, currentOrder=orderGeneration
  try {
    const value=await monitorRead<MonitorData>()
    if (current!==epoch||currentOrder!==orderGeneration) return
    lastChecked.value=value.server_time
    const signature=JSON.stringify([value.plans,value.runs,value.viewer_access,value.can_manage])
    if(signature!==displaySignature){data.value=value;displaySignature=signature}
    if(!value.can_manage){orderDialog.value?.close();order.value=undefined}
    error.value=''
    if(selected.value){
      const fresh=value.runs.find(r=>r.id===selected.value?.id)
      if(fresh){selected.value=fresh;if(fresh.has_result&&!result.value&&!resultLoading.value)void loadSelectedResult(fresh)}
      else closeRun()
    }
    const retained=new Set(value.runs.map(r=>r.id))
    resultCache.retain(retained)
    if(manual) notice.value='检测记录已更新，不会额外发起测试。'
  } catch(e) {
    if(current!==epoch) return
    error.value=e instanceof Error?e.message:'检测记录暂不可用'
    if(e instanceof MonitorError && (e.status===401||e.status===403)) clearPrivateData()
  } finally { refreshing.value=false;loadBusy=false;armPolling() }
}
function clearPrivateData() {
  epoch++;displaySignature='';clearTimeout(pollTimer);data.value=undefined;settings.value=undefined;settingsDialog.value?.close();settingsOpen.value=false;order.value=undefined;orderDialog.value?.close();orderOpen.value=false;selected.value=undefined;result.value=undefined;resultCache.clear();detail.value?.close()
  deleting.value='';deleteNotice.value=''
}
async function loadOrder() {
  orderLoading.value=true;orderError.value='';orderNotice.value=''
  const current=epoch, sequence=++orderLoadSequence
  try {
    const value=await monitorRequest<MonitorOrder>('/order')
    if(current!==epoch||sequence!==orderLoadSequence||!orderOpen.value||!data.value?.can_manage)return
    order.value=value;savedOrder.value=value.plans.map(p=>p.id)
  } catch(e) { if(current===epoch&&sequence===orderLoadSequence&&orderOpen.value)orderError.value=e instanceof Error?e.message:'顺序暂不可用' }
  finally { if(sequence===orderLoadSequence)orderLoading.value=false }
}
async function openOrder() {
  if(!data.value?.can_manage)return
  order.value=undefined;orderOpen.value=true
  await nextTick();orderDialog.value?.showModal()
  await loadOrder()
}
function movePlan(id:string, step:number) {
  const index=order.value?.plans.findIndex(p=>p.id===id)??-1
  if(index>=0)placePlan(id,index+step)
}
function placePlan(id:string, target:number) {
  if(!order.value||orderSaving.value||orderLoading.value||orderDragging.value)return
  const rows=order.value.plans, index=rows.findIndex(p=>p.id===id)
  if(index<0||target<0||target>=rows.length||index===target)return
  const [plan]=rows.splice(index,1);rows.splice(target,0,plan)
  orderNotice.value=plan.name+' 已移至第 '+(target+1)+' 位，保存后生效。'
}
function selectPlanPosition(id:string,event:Event) {
  placePlan(id,Number((event.target as HTMLSelectElement).value))
}
async function finishOrderDrag(event:DraggableEvent) {
  orderDragging.value=false
  await nextTick()
  if(event.newIndex===undefined||event.newIndex===event.oldIndex)return
  const plan=order.value?.plans[event.newIndex]
  if(plan)orderNotice.value=plan.name+' 已移至第 '+(event.newIndex+1)+' 位，保存后生效。'
}
function cancelOrder(event?:Event) {
  if(orderSaving.value||orderDragging.value){event?.preventDefault();return}
  orderDialog.value?.close()
}
async function saveOrder() {
  if(!order.value||!orderChanged.value||orderSaving.value||orderLoading.value||orderDragging.value)return
  orderSaving.value=true;orderError.value=''
  const current=epoch
  try {
    const out=await monitorRequest<{revision:number}>('/order','PUT',{revision:order.value.revision,plan_ids:order.value.plans.map(p=>p.id)})
    if(current!==epoch||!data.value?.can_manage)return
    orderGeneration++
    order.value.revision=out.revision;savedOrder.value=order.value.plans.map(p=>p.id)
    const positions=new Map(savedOrder.value.map((id,index)=>[id,index]))
    const arrange=<T extends {id:string}>(rows:T[])=>[...rows].sort((a,b)=>(positions.get(a.id)??positions.size)-(positions.get(b.id)??positions.size))
    data.value.plans=arrange(data.value.plans)
    if(settings.value)settings.value.plans=arrange(settings.value.plans)
    orderDialog.value?.close();notice.value='显示顺序已保存。糖果测试和绘图测试已同步。'
    await refresh()
  } catch(e) { if(current===epoch)orderError.value=e instanceof MonitorError&&e.status===409?'分组或顺序已更新，请点击「重新载入」后再调整。':e instanceof Error?e.message:'保存未完成，请重新载入核对。' }
  finally { orderSaving.value=false }
}
function locked() { clearPrivateData();error.value='登录状态已变化，请从主站重新打开此页面。' }
function readResult(run: MonitorRun) { return resultCache.get(run.id) }
function armPolling(){
  clearTimeout(pollTimer)
  if(!pageVisible.value||!data.value)return
  const busy=data.value.runs.some(active)
  pollTimer=setTimeout(()=>void refresh(),busy?15000:60000)
}
function visibility(){
  pageVisible.value=!document.hidden
  if(pageVisible.value)void refresh();else clearTimeout(pollTimer)
}
async function changeVisibility(event:Event){
  if(!settings.value||settingsBusy.value||saving.value)return
  const input=event.target as HTMLSelectElement
  try{await monitorRequest('/settings','PUT',{revision:settings.value.revision,viewer_access:input.value});settings.value=await monitorRequest('/settings');await refresh();notice.value='访问范围已更新。'}
  catch(e){formError.value=e instanceof Error?e.message:'保存失败';input.value=settings.value?.viewer_access||'admin'}
}
async function clearHistory(plan:AdminMonitorPlan){
  if(settingsBusy.value||saving.value||running.value)return
  const ids=(data.value?.runs||[]).filter(r=>r.plan_id===plan.id).map(r=>r.id)
  if(!ids.length||!confirm('重置「'+plan.name+'」的历史记录？系统会先备份；当前任务继续执行，配置、密钥、额度、计费和绘图轮次均保留。'))return
  settingsBusy.value=true;formError.value=''
  try{const out=await monitorRequest<{removed_ids:string[]}>('/plans/'+plan.id+'/clear-history','POST',{revision:plan.revision,run_ids:ids});settings.value=await monitorRequest('/settings');await refresh();notice.value='已清理 '+out.removed_ids.length+' 条历史记录。'}
  catch(e){formError.value=e instanceof Error?e.message:'清理未完成，请重新读取现场，不要重复提交。'}
  finally{settingsBusy.value=false}
}
async function deletePlan(plan:AdminMonitorPlan){
  if(!data.value?.can_manage||settingsBusy.value||saving.value||running.value)return
  const message='删除检测分组「'+plan.name+'」？\n\n删除后将停止自动检测，并从糖果和绘图列表移除。主站业务分组、Key #'+(plan.key_id||'待核对')+' 和已用额度保留；历史按原保留规则处理，配置留存备份。\n\n如需停用专用 Key，请另到主站 API 密钥页操作。'
  if(!confirm(message))return
  const current=epoch
  deleting.value=plan.id;settingsBusy.value=true;formError.value='';deleteNotice.value=''
  try{
    await monitorRequest('/plans/'+plan.id,'DELETE',{revision:plan.revision})
    if(current!==epoch)return
    orderGeneration++;displaySignature=''
    if(settings.value)settings.value.plans=settings.value.plans.filter(p=>p.id!==plan.id)
    if(data.value){
      data.value.plans=data.value.plans.filter(p=>p.id!==plan.id)
      data.value.runs=data.value.runs.filter(r=>r.plan_id!==plan.id)
      resultCache.retain(new Set(data.value.runs.map(r=>r.id)))
    }
    order.value=undefined
    if(group.value===plan.id)group.value='all'
    if(selected.value?.plan_id===plan.id)closeRun()
    if(tooltip.value?.run.plan_id===plan.id)tooltip.value=undefined
    if(editing.value===plan.id)newPlan()
    deleteNotice.value='已删除检测分组「'+plan.name+'」。主站业务分组和专用 Key 保留。'
    notice.value=deleteNotice.value
    await refresh()
  }catch(e){
    if(current!==epoch)return
    formError.value=e instanceof MonitorError&&e.code==='test_in_progress'?'该分组仍有未结束的检测，请先暂停自动检测，待现有任务结束后再删除。':e instanceof Error?e.message:'删除结果待核对，请重新载入配置。'
  }finally{if(current===epoch){deleting.value='';settingsBusy.value=false}}
}

async function showRun(run: MonitorRun) {
  selected.value=run;result.value=undefined;resultError.value='';view.value='fit';artRatio.value=4/3;resultLoading.value=true
  await nextTick();if(!detail.value?.open) detail.value?.showModal()
  await loadSelectedResult(run)
}
async function loadSelectedResult(run:MonitorRun){
  if(!run.has_result){resultLoading.value=false;return}
  resultLoading.value=true;resultError.value=''
  try { const value=await readResult(run);if(selected.value?.id===run.id) result.value=value }
  catch(e){if(selected.value?.id===run.id) resultError.value=e instanceof Error?e.message:'结果暂不可用'}
  finally{if(selected.value?.id===run.id) resultLoading.value=false}
}
function closeRun(){detail.value?.close();selected.value=undefined;result.value=undefined}
function adjacent(step:number){const run=selectedSiblings.value[selectedIndex.value+step];if(run)void showRun(run)}
function scrollRail(event:MouseEvent,direction:number){
  const rail=(event.currentTarget as HTMLElement).closest('.gallery-group')?.querySelector('.art-rail')
  rail?.scrollBy({left:direction*rail.clientWidth*.8,behavior:'smooth'})
}
async function openSettings(){
  settingsOpen.value=true;settingsBusy.value=true;formError.value='';deleteNotice.value=''
  await nextTick();if(!settingsDialog.value?.open)settingsDialog.value?.showModal()
  try{settings.value=await monitorRequest<MonitorSettings>('/settings');newPlan()}
  catch(e){formError.value=e instanceof Error?e.message:'配置暂不可用'}
  finally{settingsBusy.value=false}
}
function newPlan(){
  editing.value=undefined
  const next=settings.value?.groups.find(g=>!settings.value?.plans.some(p=>p.group_id===g.id))
  form.value={group_id:next?.id||0,visibility:'admin',name:next?.name||'',reasoning_effort:'medium',enabled:false,creative_every_five:false,candy_minutes:5,pelican_minutes:20,daily_limit:20,quota:200}
}
function chooseGroup(){
  form.value.name=settings.value?.groups.find(g=>g.id===Number(form.value.group_id))?.name||''
}
function editPlan(plan:AdminMonitorPlan){
  editing.value=plan.id;formError.value=''
  form.value={group_id:plan.group_id,visibility:plan.visibility||'admin',name:plan.name,reasoning_effort:plan.reasoning_effort,enabled:plan.enabled,creative_every_five:Boolean(plan.creative_every_five),candy_minutes:plan.candy_interval_seconds/60,pelican_minutes:plan.pelican_interval_seconds/60,daily_limit:plan.daily_limit,quota:plan.quota}
}
async function savePlan(){
  if(saving.value||settingsBusy.value||running.value)return
  saving.value=true;formError.value=''
  const id=editing.value
  try{
    const body={revision:originalPlan.value?.revision||0,group_id:Number(form.value.group_id),visibility:form.value.visibility,name:form.value.name,model:'gpt-6-astra',reasoning_effort:form.value.reasoning_effort,enabled:form.value.enabled,creative_every_five:form.value.creative_every_five,candy_interval_seconds:Number(form.value.candy_minutes)*60,pelican_interval_seconds:Number(form.value.pelican_minutes)*60,daily_limit:Number(form.value.daily_limit),quota:Number(form.value.quota),expires_in_days:id?0:30}
    const saved=await monitorRequest<AdminMonitorPlan>('/plans'+(id?'/'+id:''),id?'PUT':'POST',body)
    settings.value=await monitorRequest<MonitorSettings>('/settings');editPlan(saved);await refresh()
    notice.value='检测配置已保存。'
  }catch(e){formError.value=e instanceof Error?e.message:'保存未完成，请重新载入核对。'}
  finally{saving.value=false}
}
async function runNow(plan:AdminMonitorPlan,benchmark:Benchmark){
  if(settingsBusy.value||saving.value||running.value)return
  const key='sources:intelligence-manual:'+plan.id+':'+benchmark
  let requestID=pendingManual.get(key)
  try{requestID ||= sessionStorage.getItem(key)||undefined}catch{}
  requestID ||= crypto.randomUUID()
  pendingManual.set(key,requestID)
  try{sessionStorage.setItem(key,requestID)}catch{}
  running.value=plan.id+benchmark;formError.value=''
  const clearIntent=()=>{pendingManual.delete(key);try{sessionStorage.removeItem(key)}catch{}}
  try{
    await monitorRequest('/plans/'+plan.id+'/run','POST',{benchmark,request_id:requestID})
    clearIntent()
    await refresh();notice.value='检测已排队，可关闭页面后继续查看结果。'
  }catch(e){
    if(e instanceof MonitorError&&e.status<500)clearIntent()
    formError.value=e instanceof MonitorError&&e.status>=500?'提交结果待核对，请先刷新记录；再次操作将核对原请求，不会另开一轮。':e instanceof Error?e.message:'检测暂未提交'
  }finally{running.value=''}
}
function setMode(value:ColorMode){look.setMode(value)}
function setColor(value:ThemeColor){look.setColor(value)}
onMounted(()=>{
  void refresh();document.addEventListener('visibilitychange',visibility)
  clockTimer=setInterval(()=>{now.value=Date.now()},30000)
  window.addEventListener('sources:locked',locked)
})
onBeforeUnmount(()=>{clearTimeout(pollTimer);document.removeEventListener('visibilitychange',visibility);clearInterval(clockTimer);window.removeEventListener('sources:locked',locked);clearPrivateData()})
</script>

<template>
  <div class="intelligence-app" :class="{ 'is-embedded':embedded }">
    <svg class="sprite" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><defs>
      <symbol id="it-layers" viewBox="0 0 24 24"><path d="m12 3 9 5-9 5-9-5 9-5Z"/><path d="m3 12 9 5 9-5M3 16l9 5 9-5"/></symbol>
      <symbol id="it-candy" viewBox="0 0 24 24"><path d="m4 3 5 1-2 5-5-1 2-5Zm13 12 5 1-2 5-5-1 2-5Z"/><rect x="7" y="6" width="10" height="12" rx="4" transform="rotate(-40 12 12)"/></symbol>
      <symbol id="it-pen" viewBox="0 0 24 24"><path d="m15 3 6 6-12 12-7 1 1-7L15 3Z"/><path d="m12 6 6 6M3 15l6 6"/></symbol>
      <symbol id="it-refresh" viewBox="0 0 24 24"><path d="M20 7A8.5 8.5 0 0 0 5 5L2 8m0-5v5h5M4 17a8.5 8.5 0 0 0 15 2l3-3m0 5v-5h-5"/></symbol>
      <symbol id="it-arrow" viewBox="0 0 24 24"><path d="m9 5 7 7-7 7"/></symbol>
      <symbol id="it-close" viewBox="0 0 24 24"><path d="m6 6 12 12M18 6 6 18"/></symbol>
      <symbol id="it-expand" viewBox="0 0 24 24"><path d="M9 3H3v6m12-6h6v6M3 15v6h6m12-6v6h-6"/></symbol>
      <symbol id="it-grip" viewBox="0 0 24 24"><path d="M8 5h.01M16 5h.01M8 12h.01M16 12h.01M8 19h.01M16 19h.01" stroke-width="4"/></symbol>
    </defs></svg>
    <header v-if="!embedded" class="app-header">
      <div class="page-identity"><h1>智商测试</h1><p>持续观察各分组的模型表现</p></div>
      <div class="header-actions"><a href="/">Sources Code</a><button class="secondary-button" :aria-expanded="appearanceOpen" @click="appearanceOpen=!appearanceOpen">外观</button></div>
      <div v-if="appearanceOpen" class="appearance-panel">
        <div class="mode-options"><button v-for="m in (['light','dark','system'] as const)" :key="m" :aria-pressed="appearanceState.mode===m" @click="setMode(m)">{{ ({light:'亮色',dark:'暗色',system:'跟随系统'})[m] }}</button></div>
        <div class="palette-options"><button v-for="p in palettes" :key="p.key" :aria-label="p.name" :aria-pressed="appearanceState.color===p.key" :title="p.name" :style="{backgroundColor:p.colors[500]}" @click="setColor(p.key)"><span v-if="appearanceState.color===p.key">✓</span></button></div>
        <p v-if="!appearanceState.storageAvailable" class="muted">当前外观仅本次有效。</p>
      </div>
    </header>
    <main>
      <div v-if="!data" class="access-state" role="status"><h2>{{ error?'暂时无法查看':'正在读取检测记录…' }}</h2><p>{{error||'请稍候'}}</p><a v-if="error" class="secondary-button" href="/login?redirect=%2Fintelligence-tests%3Fmanage%3D1">前往主站登录</a><button v-if="error" class="text-button" @click="refresh(true)">重试</button></div>
      <template v-else>
        <p v-if="error" class="inline-notice" role="status">{{error}}，当前保留上次记录。</p>
        <p v-if="notice" class="inline-notice" role="status">{{notice}}<button class="text-button" @click="notice=''">关闭</button></p>
        <section id="candy">
          <div class="section-heading"><div class="section-title"><svg><use href="#it-candy"/></svg><h2>糖果测试</h2><span class="badge">{{cards.length}} 个分组</span><span class="badge">最近 60 次</span><span v-if="data.viewer_access==='admin'" class="admin-badge">仅管理员可见</span></div><div class="section-actions"><div class="legend"><span><i class="passed"></i>答对</span><span><i class="incorrect"></i>答错</span><span><i class="running"></i>测试中</span><span><i class="error"></i>失败 / 未知</span><span><i class="empty"></i>暂无记录</span></div><button v-if="data.can_manage&&cards.length>1" class="secondary-button" @click="openOrder">调整顺序</button><button v-if="data.can_manage" class="secondary-button" @click="openSettings">检测设置</button></div></div>
          <div v-if="!cards.length" class="empty-state">尚未配置检测分组。<button v-if="data.can_manage" class="text-button" @click="openSettings">添加检测分组</button></div>
          <div class="candy-grid">
            <article v-for="plan in cards" :key="plan.id" class="candy-card">
              <div class="candy-card-head"><div class="group-icon"><svg><use href="#it-layers"/></svg></div><div class="group-title"><h3 :title="plan.name">{{plan.name}}</h3><span v-if="data.can_manage&&plan.visibility!=='authenticated'" class="badge">私有测试</span><p class="model-line">{{plan.model}} <span class="effort">{{plan.reasoning_effort}}</span><span v-if="!plan.enabled" class="paused">已暂停</span></p></div></div>
              <div class="metrics"><div><div class="metric-value success">{{stats(plan).pass}}<em v-if="stats(plan).valid">%</em></div><div class="metric-label">通过率</div></div><div><div class="metric-value">{{duration(stats(plan).latest).value}}<em>{{duration(stats(plan).latest).unit}}</em></div><div class="metric-label">最近耗时</div></div><div><div class="metric-value">{{stats(plan).completed}}<em>次</em></div><div class="metric-label">完成次数</div></div></div>
              <div class="history-label"><span :class="{'is-running':stats(plan).pending}">{{stats(plan).pending?stats(plan).pending+' 项测试中':stats(plan).completed?'最近一轮已结束':'等待首次检测'}}</span><span>有效 {{stats(plan).valid}} 次 · 异常 {{stats(plan).failed}} 次</span></div>
              <div class="history-bars" :aria-label="plan.name+'最近60次检测'"><button v-for="(run,index) in bars(plan)" :key="run?.id||index" class="history-bar" :class="status(run)" :aria-label="run?stamp(run.started_at)+' '+verdict(run):'暂无记录'" @mouseenter="showTooltip($event,run)" @focus="showTooltip($event,run)" @mouseleave="tooltip=undefined" @blur="tooltip=undefined" @click="run&&showRun(run)"></button></div>
              <div class="history-footer"><span>{{stamp(stats(plan).first?.started_at,true)}}</span><span>{{interval(plan.candy_interval_seconds)}}</span><span>最新 · {{stamp(stats(plan).last?.started_at,true)}}</span></div>
            </article>
          </div>
          <div class="section-note"><p>通过率仅统计已判定答案，测试失败与判定未知单独记录。</p><button class="text-button" @click="methodOpen=true;method?.showModal()">测试题与判定依据 ↗</button></div>
        </section>
        <section id="pelican">
          <div class="section-heading gallery-heading"><div class="section-title"><svg><use href="#it-pen"/></svg><h2>绘图测试</h2></div><span class="muted gallery-subtitle">固定题作对照，自由发挥看创作</span></div>
          <div class="gallery-toolbar"><div class="group-tabs" role="tablist" aria-label="筛选分组"><button role="tab" :aria-selected="group==='all'" :class="{active:group==='all'}" @click="group='all'">全部分组</button><button v-for="plan in cards" :key="plan.id" role="tab" :aria-selected="group===plan.id" :class="{active:group===plan.id}" @click="group=plan.id">{{plan.name}} <span>{{works(plan).length}}</span></button></div><div class="gallery-controls"><select v-model="drawingFilter" aria-label="绘图类型"><option value="all">全部类型</option><option value="pelican">鹈鹕</option><option value="creative">自由发挥</option></select><select v-model="filter" aria-label="作品状态"><option value="all">全部状态</option><option value="normal">本次正常</option><option value="degraded">本次需关注</option><option value="unknown">无法判定</option><option value="failed">生成失败</option></select><button class="icon-button" :disabled="refreshing" aria-label="刷新记录" @click="refresh(true)"><svg><use href="#it-refresh"/></svg></button></div></div>
          <div class="gallery-meta"><span>每组保留最近 20 张 · 超过 7 天自动清理</span><span>更新于 {{stamp(data.server_time,true)}}</span></div>
          <section v-for="plan in galleries" :key="plan.id" class="gallery-group">
            <div class="gallery-group-heading"><div class="gallery-group-identity"><div class="group-icon"><svg><use href="#it-layers"/></svg></div><div class="group-title"><h3>{{plan.name}}<span v-if="data.can_manage&&plan.visibility!=='authenticated'" class="badge">私有测试</span></h3><p>{{plan.model}} · {{interval(plan.pelican_interval_seconds)}} · {{ago(works(plan)[0]?.finished_at)}}<span v-if="plan.enabled&&plan.pelican_interval_seconds"> · 下一次：{{drawingLabel(plan.next_drawing_benchmark)}}</span></p></div></div><div class="rail-buttons"><button class="icon-button previous" :aria-label="plan.name+'向左滚动'" @click="scrollRail($event,-1)"><svg><use href="#it-arrow"/></svg></button><button class="icon-button" :aria-label="plan.name+'向右滚动'" @click="scrollRail($event,1)"><svg><use href="#it-arrow"/></svg></button></div></div>
            <div class="art-rail">
              <div v-if="!works(plan).length" class="empty-state">暂无符合条件的作品{{plan.enabled?'，完成检测后会自动展示。':'，该分组当前已暂停。'}}</div>
              <article v-for="run in works(plan)" :key="run.id" class="art-card">
                <div class="art-media"><IntelligenceArtwork v-if="run.has_html" thumbnail :suspended="previewsSuspended" :load="()=>readResult(run).then(r=>r.html)" :label="plan.name+drawingLabel(run.benchmark)+'作品'" /><div v-else class="failed-preview"><span>{{active(run)?verdict(run):'本次未产生作品'}}</span><small>{{active(run)?'关闭页面也会继续检测':'已保留检测记录'}}</small></div><span v-if="!active(run)&&run.verdict!=='normal'" class="art-status">{{verdict(run)}}</span><button class="art-overlay" :aria-label="'查看 '+plan.name+' '+stamp(run.started_at)+' 作品'" @click="showRun(run)"><span class="zoom"><svg><use href="#it-expand"/></svg></span></button></div>
                <div class="art-meta"><span class="drawing-kind" :class="{creative:run.benchmark==='creative'}">{{drawingLabel(run.benchmark)}}</span><div class="art-meta-top"><strong>{{run.model}}</strong><span class="effort">思考强度 {{run.reasoning_effort==='medium'?'中':run.reasoning_effort==='low'?'低':'高'}}</span></div><div class="art-meta-bottom"><time>{{stamp(run.started_at)}}</time><span>耗时 {{duration(run).value}} {{duration(run).unit}}</span></div></div>
              </article>
            </div>
          </section>
        </section>
        <footer><span>检测仅反映本次题目表现，不用于证明模型身份或整体能力。</span><button class="text-button" @click="methodOpen=true;method?.showModal()">查看判定说明 ↗</button></footer>
      </template>
    </main>
    <div v-if="tooltip" class="bar-tooltip" role="tooltip" :style="{left:tooltip.x+'px',top:tooltip.y+'px'}"><strong>{{verdict(tooltip.run)}}</strong><p>{{stamp(tooltip.run.started_at)}}</p><p>{{tooltip.run.model}} · {{tooltip.run.reasoning_effort}}</p><p>耗时 {{duration(tooltip.run).value}} {{duration(tooltip.run).unit}} · 点击查看详情</p></div>
    <dialog ref="detail" class="detail-modal" :class="{'answer-modal':selected?.benchmark==='candy'}" :style="{'--art-ratio':artRatio}" @close="selected=undefined;result=undefined">
      <template v-if="selected"><div class="dialog-header"><div><p class="eyebrow">{{selected.benchmark==='candy'?'糖果检测结果':drawingLabel(selected.benchmark)+'作品'}}</p><h3>{{selectedPlan?.name||selected.name}}</h3></div><button class="icon-button" aria-label="关闭结果" @click="closeRun"><svg><use href="#it-close"/></svg></button></div>
        <div class="detail-toolbar"><div class="detail-meta"><span>{{stamp(selected.started_at)}}</span><span>{{selected.model}} · {{selected.reasoning_effort}}</span><span>耗时 {{duration(selected).value}} {{duration(selected).unit}}</span><span>{{verdict(selected)}}</span></div><div v-if="selected.has_html" class="preview-modes" role="group" aria-label="作品显示比例"><button :aria-pressed="view==='fit'" @click="view='fit'">适应窗口</button><button :aria-pressed="view==='actual'" @click="view='actual'">100%</button></div></div>
        <div v-if="selected.benchmark!=='candy'" class="detail-preview"><IntelligenceArtwork v-if="result?.html" :html="result.html" :suspended="!pageVisible" :mode="view" @size="artRatio=$event.width/$event.height"/><div v-else class="failed-preview">{{resultLoading?'正在读取作品…':resultError||'本次未产生可展示作品。'}}</div></div>
        <div v-else class="answer-content"><p v-if="resultLoading">正在读取回答…</p><p v-else-if="resultError">{{resultError}}</p><pre v-else>{{result?.answer||'尚无可展示的回答。'}}</pre></div>
        <div class="dialog-footer"><span>{{view==='fit'?'完整适应窗口':'原始尺寸，可滚动查看细节'}}<template v-if="selected.input_tokens!==undefined"> · 输入 {{selected.input_tokens}} Token</template><template v-if="selected.output_tokens!==undefined"> · 输出 {{selected.output_tokens}} Token</template></span><div><button class="secondary-button" :disabled="selectedIndex<=0" @click="adjacent(-1)">← 上一条</button><button class="secondary-button" :disabled="selectedIndex>=selectedSiblings.length-1" @click="adjacent(1)">下一条 →</button></div></div>
      </template>
    </dialog>
    <dialog ref="method" class="method-modal" @close="methodOpen=false"><div class="dialog-header"><h3>测试题与判定依据</h3><button class="icon-button" aria-label="关闭判定说明" @click="method?.close()"><svg><use href="#it-close"/></svg></button></div><div class="method-body"><h4>糖果测试</h4><p>固定题目：黑袋中有苹果、桃子、西瓜三种口味，以及圆形、五角星两种可凭手感分辨的形状。活动前确定取出数量，求保证获得不同形状的苹果味与桃子味糖果所需的最少数量。</p><table><thead><tr><th>形状</th><th>苹果味</th><th>桃子味</th><th>西瓜味</th></tr></thead><tbody><tr><th>圆形</th><td>7</td><td>9</td><td>8</td></tr><tr><th>五角星</th><td>7</td><td>6</td><td>4</td></tr></tbody></table><p>采用固定题目检测协议；通过率 = 答对 ÷（答对 + 答错）。失败、超时或无法判定单独记录；没有有效答案时显示「—」。点击状态条可查看当次回答。</p><h4>绘图测试</h4><p>鹈鹕使用固定题目作对照；启用混合模式后，每 5 次自动绘图的第 5 次为自由发挥。失败占本轮且不额外补发，手动测试不占轮次。自由作品展示实际创作及参考评估，不纳入糖果通过率。作品在不能读取主站登录、不能联网的隔离预览中运行。</p><p>本页展示请求的模型与参数。单题表现、作品风格或自动评估不能证明模型身份或整体能力。</p></div></dialog>
    <dialog ref="orderDialog" class="order-dialog" aria-labelledby="order-title" @cancel="cancelOrder($event)" @close="orderOpen=false">
      <div class="dialog-header"><div><p class="eyebrow">管理员 · {{order?.plans.length??cards.length}} 个分组</p><h3 id="order-title">调整显示顺序</h3></div><button class="icon-button" aria-label="关闭排序" :disabled="orderSaving||orderDragging" @click="cancelOrder()"><svg><use href="#it-close"/></svg></button></div>
      <div class="order-body" :aria-busy="orderLoading||orderSaving">
        <p id="order-help" class="settings-note">拖动左侧手柄调整顺序，保存后同步到糖果和绘图。也可选择右侧位置，或聚焦手柄按 Alt + ↑ / ↓。</p>
        <p v-if="orderLoading" role="status">正在读取顺序…</p>
        <p v-if="orderError" class="form-error" role="alert">{{orderError}}</p>
        <VueDraggable v-if="order" v-model="order.plans" tag="ol" class="order-list" aria-label="检测分组顺序" :animation="orderAnimation" :disabled="orderSaving||orderLoading" handle=".order-handle" ghost-class="order-row-ghost" chosen-class="order-row-chosen" :scroll-sensitivity="60" :scroll-speed="12" @start="orderDragging=true" @end="finishOrderDrag">
          <li v-for="(plan,index) in order.plans" :key="plan.id" :data-plan-id="plan.id" class="order-row">
            <button type="button" class="order-handle" :aria-label="'拖动 '+plan.name" aria-describedby="order-help" :disabled="orderSaving||orderLoading" @keydown.alt.up.prevent="movePlan(plan.id,-1)" @keydown.alt.down.prevent="movePlan(plan.id,1)"><svg aria-hidden="true"><use href="#it-grip"/></svg></button>
            <strong>{{plan.name}}</strong>
            <select class="order-position-select" :aria-label="'设置 '+plan.name+' 的位置'" :value="index" :disabled="orderSaving||orderLoading||orderDragging" @change="selectPlanPosition(plan.id,$event)"><option v-for="(_,position) in order.plans" :key="position" :value="position">第 {{position+1}} 位</option></select>
          </li>
        </VueDraggable>
        <p class="order-notice" role="status" aria-live="polite">{{orderNotice||'调整完成后点击「保存顺序」。'}}</p>
      </div>
      <div class="order-footer"><button class="text-button" :disabled="orderSaving||orderLoading||orderDragging" @click="loadOrder">重新载入</button><div><button class="secondary-button" :disabled="orderSaving||orderDragging" @click="cancelOrder()">取消</button><button class="primary-button" :disabled="!orderChanged||orderSaving||orderLoading||orderDragging" @click="saveOrder">{{orderSaving?'正在保存…':'保存顺序'}}</button></div></div>
    </dialog>
    <dialog ref="settingsDialog" class="settings-drawer" aria-label="检测设置" @close="settingsOpen=false"><div class="dialog-header"><div><p class="eyebrow">管理员</p><h3>检测设置</h3></div><button class="icon-button" aria-label="关闭检测设置" @click="settingsDialog?.close()"><svg><use href="#it-close"/></svg></button></div>
      <div v-if="settingsOpen" class="settings-body"><p v-if="settingsBusy&&!settings">正在读取配置…</p><p v-if="deleteNotice" class="settings-note" role="status">{{deleteNotice}}</p><p v-if="formError" class="form-error" role="alert">{{formError}}</p><template v-if="settings">
        <p class="settings-note">当前{{settings.viewer_access==='admin'?'仅管理员可见':'登录用户只读可见'}}。使用本站专用限额 Key 进行检测，按主站正常价格扣费；不会使用业务 Key 或自动补充余额。</p>
        <label class="settings-note">访问范围 <select :value="settings.viewer_access" aria-label="访问范围" :disabled="settingsBusy||saving||Boolean(running)" @change="changeVisibility"><option value="admin">仅管理员</option><option value="authenticated">登录用户只读</option></select></label><div class="plan-list"><div v-for="plan in settings.plans" :key="plan.id" class="plan-row"><button class="plan-select" :disabled="settingsBusy||saving" @click="editPlan(plan)"><strong>{{plan.name}}</strong><span>{{plan.visibility==='authenticated'?'登录用户可见':'仅管理员可见'}} · {{plan.enabled?'自动检测中':'已暂停'}} · {{plan.key_status==='active'?'密钥可用':'密钥需检查'}}</span></button><div><button class="text-button" :disabled="settingsBusy||saving||Boolean(running)" @click="runNow(plan,'candy')">测糖果</button><button class="text-button" :disabled="settingsBusy||saving||Boolean(running)" @click="runNow(plan,'pelican')">测鹈鹕</button><button class="text-button" :disabled="settingsBusy||saving||Boolean(running)" @click="runNow(plan,'creative')">自由发挥</button><button class="text-button" :disabled="settingsBusy||saving||Boolean(running)" @click="clearHistory(plan)">重置记录</button><button class="text-button delete-plan-button" :aria-label="'删除检测分组 '+plan.name" :disabled="settingsBusy||saving||Boolean(running)" @click="deletePlan(plan)">{{deleting===plan.id?'正在删除…':'删除分组'}}</button></div></div><button class="secondary-button" :disabled="settingsBusy||saving" @click="newPlan">＋ 添加分组</button></div>
        <p class="settings-note">私有测试先在主站配置账号的 URL 和 Key，并使用仅授权测试身份的专属分组。这里只选择分组，检测服务收到的是主站地址及专用限额 Key。</p><p class="settings-note"><a href="/admin/accounts" target="_top">账号管理</a> · <a href="/admin/groups" target="_top">分组管理</a></p><form class="monitor-form" @submit.prevent="savePlan"><h4>{{editing?'编辑检测计划':'新建检测计划'}}</h4><label>主站分组<select aria-label="主站分组" v-model="form.group_id" :disabled="Boolean(editing)" @change="chooseGroup"><option v-for="g in settings.groups" :key="g.id" :value="g.id">{{g.name}}{{g.is_exclusive?' · 专属':''}}</option></select></label><label>展示名称<input v-model="form.name" maxlength="60" required></label><label>本组可见范围<select v-model="form.visibility" aria-label="本组可见范围"><option value="admin">仅管理员（私有测试）</option><option value="authenticated">登录用户只读</option></select></label><div class="form-grid"><label>模型<input value="gpt-6-astra" readonly></label><label>推理强度<select v-model="form.reasoning_effort"><option value="low">low</option><option value="medium">medium</option><option value="high">high</option></select></label></div><div class="form-grid"><label>糖果间隔（分钟）<input v-model="form.candy_minutes" type="number" min="0" max="1440" step="1" required></label><label>绘图间隔（分钟）<input v-model="form.pelican_minutes" type="number" min="0" max="1440" step="1" required></label></div><p class="field-help">填 0 可关闭该项；正在进行的任务会继续查询，结果不明不会自动重发。</p><div class="form-grid"><label>滚动 24 小时限额<input v-model="form.daily_limit" type="number" min="0.01" step="0.01" required></label><label>累计总额度<input v-model="form.quota" type="number" min="0" step="0.01" required></label></div><p class="field-help">单位为主站余额，不是采购成本。累计总额度填 0 表示不限；每日限额须大于 0。调整限额不会重置已用额度。</p><p v-if="originalPlan" class="field-help">Key #{{originalPlan.key_id||'待核对'}} · {{originalPlan.expires_at?'有效至 '+stamp(originalPlan.expires_at):'永久有效'}}；有效期沿用主站 Key 设置。</p><p v-else class="field-help">新测试默认仅管理员可见、自动检测关闭。专用 Key 有效期 30 天，可在主站 API 密钥页撤销。</p><label class="checkbox-label"><input v-model="form.creative_every_five" type="checkbox">每 5 次绘图穿插 1 次自由发挥</label><p class="field-help">按 4 次鹈鹕＋1 次自由发挥循环，手动测试不占轮次。</p><label class="checkbox-label"><input v-model="form.enabled" type="checkbox">启用自动检测</label><p class="field-help">检测由外部检测服务执行，仅向其提供本站专用限额 Key；后台保存结果。</p><button class="primary-button" :disabled="saving||settingsBusy||Boolean(running)||!form.group_id">{{saving?'正在保存…':editing?'保存配置':'创建专用 Key 并保存'}}</button></form>
      </template></div>
    </dialog>
  </div>
</template>
