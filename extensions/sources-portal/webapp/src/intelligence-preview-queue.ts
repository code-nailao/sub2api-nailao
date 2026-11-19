// Spread document initialization over frames. Artwork animation APIs, clocks,
// canvas resolution and frame rate are never wrapped or modified.
const pending = new Map<number, () => void>()
let sequence = 0, frame: number | undefined
function schedule() {
  if (frame !== undefined || !pending.size) return
  frame = requestAnimationFrame(() => {
    frame = undefined
    const next = pending.entries().next().value
    if (next) { pending.delete(next[0]); next[1]() }
    schedule()
  })
}
export function schedulePreview(create: () => void) {
  const id = ++sequence
  pending.set(id, create); schedule()
  return () => {
    pending.delete(id)
    if (!pending.size && frame !== undefined) { cancelAnimationFrame(frame); frame = undefined }
  }
}
