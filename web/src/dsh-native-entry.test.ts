// @vitest-environment jsdom

import { afterEach, describe, expect, it } from 'vitest'
import { installDshNativeAccessibilityBridge } from './dsh-native-entry'

describe('DSH native accessibility bridge', () => {
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
      const cleanup = installDshNativeAccessibilityBridge(document)

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
})
