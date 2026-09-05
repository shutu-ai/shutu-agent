import { mountShutuNativeApp } from './native-entry'

const root = document.getElementById('root')
if (root === null) throw new Error('shutu web: missing #root')

void mountShutuNativeApp(root).catch((error: unknown) => {
  const message = error instanceof Error ? error.message : String(error)
  root.replaceChildren(Object.assign(document.createElement('pre'), { textContent: message }))
  console.error(error)
})
