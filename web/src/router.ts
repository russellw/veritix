/*
A router in about sixty lines, in place of a dependency.

The usual argument for taking react-router is that a hand-rolled router stays
small only until you need route params, nested layouts, scroll restoration and
navigation guards. Veritix needs none of those: five flat routes, one of them
parameterized, and no forms to guard. The dependency would be four packages and
three maintainers in a bundle whose entire point is that it handles commercially
sensitive data — see docs/frontend-stack.md.

Real URLs rather than hashes, because a finding's id is stable across runs and
`/runs/{id}/findings/{fid}` is therefore worth being able to send to somebody.
The Go handler falls back to index.html so those paths survive a reload.
*/

import { useSyncExternalStore } from 'react'

const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

function subscribe(onChange: () => void) {
  listeners.add(onChange)
  return () => {
    listeners.delete(onChange)
  }
}

// The snapshot has to be a primitive: useSyncExternalStore compares it by
// identity, and returning a fresh {pathname} object every call would re-render
// forever.
function snapshot() {
  return window.location.pathname
}

window.addEventListener('popstate', emit)

export function navigate(to: string, replace = false) {
  if (to === window.location.pathname) return
  if (replace) window.history.replaceState(null, '', to)
  else window.history.pushState(null, '', to)
  emit()
  window.scrollTo(0, 0)
}

export function usePath(): string {
  return useSyncExternalStore(subscribe, snapshot)
}

/**
 * match tests a path against a pattern such as `/runs/:id/findings/:fid`,
 * returning the captured parameters, or null when it does not match.
 */
export function match(pattern: string, path: string): Record<string, string> | null {
  const want = pattern.split('/')
  const got = path.split('/')
  if (want.length !== got.length) return null

  const params: Record<string, string> = {}
  for (let i = 0; i < want.length; i++) {
    if (want[i].startsWith(':')) {
      if (got[i] === '') return null
      params[want[i].slice(1)] = decodeURIComponent(got[i])
    } else if (want[i] !== got[i]) {
      return null
    }
  }
  return params
}

/**
 * onLinkClick lets a plain <a> navigate in-app while still behaving like a
 * link. Modified clicks, middle clicks and anything a handler has already dealt
 * with fall through to the browser, so open-in-new-tab keeps working.
 */
export function onLinkClick(to: string) {
  return (e: React.MouseEvent<HTMLAnchorElement>) => {
    if (e.defaultPrevented || e.button !== 0) return
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
    e.preventDefault()
    navigate(to)
  }
}
