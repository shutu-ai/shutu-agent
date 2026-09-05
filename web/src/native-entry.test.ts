// @vitest-environment jsdom

import { afterEach, describe, expect, it } from 'vitest'
import { installShutuNativeAccessibilityBridge } from './native-entry'

describe('SHUTU native accessibility bridge', () => {
  afterEach(() => {
    document.body.innerHTML = ''
  })

  it('labels icon-only dialog triggers and restores focus after the dialog closes', async () => {
    const requestAnimationFrame = globalThis.requestAnimationFrame
    globalThis.requestAnimationFrame = (callback: FrameRequestCallback): number => {
      callback(0)
      return 1
    }
    try {
      const button = document.createElement('button')
      button.type = 'button'
      button.setAttribute('aria-haspopup', 'dialog')
      document.body.append(button)
      const cleanup = installShutuNativeAccessibilityBridge(document)

      expect(button.getAttribute('aria-label')).toBe('Settings')
      button.click()
      const dialog = document.createElement('div')
      dialog.setAttribute('role', 'dialog')
      document.body.append(dialog)
      await Promise.resolve()
      dialog.remove()
      await Promise.resolve()

      expect(document.activeElement).toBe(button)
      cleanup()
    } finally {
      globalThis.requestAnimationFrame = requestAnimationFrame
    }
  })

  it('keeps Tab focus inside the native dialog', () => {
    document.body.innerHTML = '<button id="trigger" aria-haspopup="dialog"></button>'
    const cleanup = installShutuNativeAccessibilityBridge(document)
    const dialog = document.createElement('div')
    dialog.setAttribute('role', 'dialog')
    dialog.innerHTML = '<button id="first">First</button><button id="last">Last</button>'
    document.body.append(dialog)
    const first = document.querySelector<HTMLButtonElement>('#first')
    const last = document.querySelector<HTMLButtonElement>('#last')
    if (first === null || last === null) throw new Error('test dialog controls were not mounted')

    last.focus()
    last.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', bubbles: true, cancelable: true }))
    expect(document.activeElement).toBe(first)
    first.dispatchEvent(new KeyboardEvent('keydown', { key: 'Tab', shiftKey: true, bubbles: true, cancelable: true }))
    expect(document.activeElement).toBe(last)
    cleanup()
  })

  it('moves focus into the first dialog control when it opens', async () => {
    const trigger = document.createElement('button')
    trigger.setAttribute('aria-haspopup', 'dialog')
    document.body.append(trigger)
    const cleanup = installShutuNativeAccessibilityBridge(document)
    trigger.focus()
    const dialog = document.createElement('div')
    dialog.setAttribute('role', 'dialog')
    dialog.innerHTML = '<button id="first">First</button><button id="last">Last</button>'
    document.body.append(dialog)
    await Promise.resolve()

    expect(document.activeElement).toBe(document.querySelector('#first'))
    cleanup()
  })
})
