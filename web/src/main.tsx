import { createRoot } from 'react-dom/client'
import { Context } from '@deepseek-ai/cordis'
import { ShutuApi } from './api'
import { App } from './app'
import { WebStore } from './store'

const root = document.getElementById('root')
if (root === null) throw new Error('shutu web: missing #root')

const savedToken = typeof localStorage === 'undefined' ? '' : localStorage.getItem('shutu.web.token') ?? ''
const api = new ShutuApi('', savedToken)
const store = new WebStore(api)
const ctx = new Context()
ctx.reflect.provide('shutuApi', api)
ctx.reflect.provide('sessions', store)

createRoot(root).render(<App store={ctx.get('sessions') as WebStore} />)
