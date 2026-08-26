import { createRoot } from 'react-dom/client'
import { Context } from '@deepseek-ai/cordis'
import { ShutuApi } from './api'
import { App } from './app'
import { installDshNativeBoot, isDshNativeBuild, mountDshNativeApp } from './dsh-native-entry'
import { WebStore } from './store'

const root = document.getElementById('root')
if (root === null) throw new Error('shutu web: missing #root')

if (isDshNativeBuild()) {
  installDshNativeBoot()
  void mountDshNativeApp(root).catch((error: unknown) => {
    const message = error instanceof Error ? error.message : String(error)
    root.replaceChildren(Object.assign(document.createElement('pre'), { textContent: message }))
    console.error(error)
  })
} else {

  const savedToken = typeof localStorage === 'undefined' ? '' : localStorage.getItem('shutu.web.token') ?? ''
  const api = new ShutuApi('', savedToken)
  const store = new WebStore(api)
  const ctx = new Context()
  ctx.reflect.provide('shutuApi', api)
  ctx.reflect.provide('sessions', store)

  createRoot(root).render(<App store={ctx.get('sessions') as WebStore} />)
}
