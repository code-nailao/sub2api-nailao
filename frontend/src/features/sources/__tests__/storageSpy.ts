import { vi } from 'vitest'

// jsdom Storage is an exotic object: replacing an instance method can silently
// become a stored key. Node's fallback test Storage owns ordinary methods.
export function spyOnStorage<K extends 'getItem' | 'setItem'>(method: K) {
  const storage = window.localStorage
  const owner: Storage = Object.prototype.hasOwnProperty.call(storage, method)
    ? storage
    : Object.getPrototypeOf(storage)
  return vi.spyOn(owner, method)
}
