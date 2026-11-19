import type { MonitorResult } from './intelligence-types'

type Cached = { promise: Promise<MonitorResult>; bytes: number }
type Work = { id: string; epoch: number; promise: Promise<MonitorResult>; resolve: (v: MonitorResult) => void; reject: (e: Error) => void }

// Reads existing results only; auth, snapshots and paid submission use other paths.
export function createResultCache(load: (id: string) => Promise<MonitorResult>, limits = { concurrent: 6, entries: 60, bytes: 16 * 1024 * 1024 }) {
  const cache = new Map<string, Cached>(), pending = new Map<string, Work>(), queue: Work[] = []
  let active = 0, epoch = 0, bytes = 0
  function remove(id: string) { const e = cache.get(id); if (e) { bytes -= e.bytes; cache.delete(id) } }
  function pump() {
    while (active < limits.concurrent && queue.length) {
      const work = queue.shift()!
      if (work.epoch !== epoch || pending.get(work.id) !== work) { work.reject(new Error('记录已失效')); continue }
      active++
      void load(work.id).then(value => {
        if (work.epoch !== epoch || pending.get(work.id) !== work) throw new Error('记录已失效')
        const size = new TextEncoder().encode(JSON.stringify(value)).byteLength
        if (size <= limits.bytes) {
          cache.set(work.id, { promise: work.promise, bytes: size }); bytes += size
          while (cache.size > limits.entries || bytes > limits.bytes) remove(cache.keys().next().value!)
        }
        work.resolve(value)
      }).catch(e => work.reject(e)).finally(() => {
        if (pending.get(work.id) === work) pending.delete(work.id)
        active--; pump()
      })
    }
  }
  return {
    get(id: string) {
      const old = cache.get(id)
      if (old) { cache.delete(id); cache.set(id, old); return old.promise }
      const waiting = pending.get(id); if (waiting) return waiting.promise
      let resolve!: Work['resolve'], reject!: Work['reject']
      const promise = new Promise<MonitorResult>((a, b) => { resolve = a; reject = b })
      const work = { id, epoch, promise, resolve, reject }
      pending.set(id, work); queue.push(work); pump(); return promise
    },
    retain(ids: Set<string>) {
      for (const id of cache.keys()) if (!ids.has(id)) remove(id)
      for (const [id, work] of pending) if (!ids.has(id)) { pending.delete(id); work.reject(new Error('记录已清理')) }
      pump()
    },
    clear() {
      epoch++; cache.clear(); bytes = 0
      for (const work of pending.values()) work.reject(new Error('登录状态已变化'))
      pending.clear(); queue.splice(0)
    },
  }
}
