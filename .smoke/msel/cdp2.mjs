import { spawn } from 'node:child_process'
import http from 'node:http'

const CHROME = process.env.CHROME
const PROFILE = process.env.PROFILE
const PAGE = process.env.PAGE
const PORT = 9230

function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, (res) => {
      let data = ''
      res.on('data', (c) => { data += c })
      res.on('end', () => {
        try { resolve(JSON.parse(data)) } catch (e) { reject(e) }
      })
    }).on('error', reject)
  })
}

class WS {
  constructor(url) { this.url = url; this.id = 0; this.pending = new Map(); this.events = new Map() }
  connect() {
    return new Promise((resolve, reject) => {
      const u = new URL(this.url)
      const key = Buffer.from(Math.random().toString(36)).toString('base64')
      const req = http.request({ host: u.hostname, port: u.port, path: u.pathname + u.search, headers: { Connection: 'Upgrade', Upgrade: 'websocket', 'Sec-WebSocket-Key': key, 'Sec-WebSocket-Version': '13' } })
      req.on('upgrade', (res, socket) => { this.socket = socket; socket.on('data', (buf) => this._onData(buf)); resolve() })
      req.on('error', reject)
      req.end()
    })
  }
  _onData(buf) {
    this._rem = this._rem ? Buffer.concat([this._rem, buf]) : buf
    while (this._rem.length >= 2) {
      const b0 = this._rem[0]; const opcode = b0 & 0x0f; const b1 = this._rem[1]; const len = b1 & 0x7f
      let offset = 2; let payloadLen = len
      if (len === 126) { if (this._rem.length < 4) return; payloadLen = this._rem.readUInt16BE(2); offset = 4 }
      else if (len === 127) { if (this._rem.length < 10) return; payloadLen = Number(this._rem.readBigUInt64BE(2)); offset = 10 }
      if (this._rem.length < offset + payloadLen) return
      const payload = this._rem.subarray(offset, offset + payloadLen)
      this._rem = this._rem.subarray(offset + payloadLen)
      if (opcode === 1) {
        const msg = JSON.parse(payload.toString('utf8'))
        if (msg.id !== undefined) { const p = this.pending.get(msg.id); if (p) { this.pending.delete(msg.id); p.resolve(msg) } }
        else if (msg.method) { const list = this.events.get(msg.method); if (list) for (const fn of list) fn(msg.params) }
      } else if (opcode === 8) { this._rem = Buffer.alloc(0) }
    }
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      const payload = JSON.stringify({ id, method, params })
      const body = Buffer.from(payload, 'utf8')
      const mask = Buffer.from([1, 2, 3, 4])
      let header
      if (body.length < 126) header = Buffer.from([0x81, 0x80 | body.length])
      else if (body.length < 65536) { header = Buffer.alloc(4); header[0] = 0x81; header[1] = 0x80 | 126; header.writeUInt16BE(body.length, 2) }
      else { header = Buffer.alloc(10); header[0] = 0x81; header[1] = 0x80 | 127; header.writeBigUInt64BE(BigInt(body.length), 2) }
      const masked = Buffer.alloc(body.length)
      for (let i = 0; i < body.length; i++) masked[i] = body[i] ^ mask[i % 4]
      this.socket.write(Buffer.concat([header, mask, masked]))
    })
  }
  on(method, fn) { if (!this.events.has(method)) this.events.set(method, []); this.events.get(method).push(fn) }
  close() { try { this.socket.end() } catch {} }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
async function waitFor(fn, timeout = 15000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    try { const v = await fn(); if (v) return v } catch {}
    await sleep(100)
  }
  throw new Error('timeout')
}

async function main() {
  const chrome = spawn(CHROME, ['--headless=new', '--disable-gpu', '--no-first-run', '--remote-debugging-port=' + PORT, `--user-data-dir=${PROFILE}`, PAGE], { stdio: 'ignore' })
  try {
    const target = await waitFor(async () => {
      const list = await getJSON(`http://127.0.0.1:${PORT}/json`)
      return list.find((t) => t.type === 'page')
    })
    const ws = new WS(target.webSocketDebuggerUrl)
    await ws.connect()
    await ws.send('Runtime.enable')
    await sleep(2500)
    const evalJS = async (expr) => {
      const res = await ws.send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })
      if (res.result && res.result.exceptionDetails) throw new Error('eval: ' + JSON.stringify(res.result.exceptionDetails))
      return res.result ? res.result.result.value : undefined
    }
    const out = {}
    out.seatText = await evalJS(`(document.querySelector('#model-seat-label') || {}).textContent`)
    out.seatDisabled = await evalJS(`(document.querySelector('#model-seat') || {}).disabled`)
    // The app reads currentID from localStorage KEY_CURRENT; force a session
    // and re-run the model-seat wiring by clicking after unlocking.
    await evalJS(`localStorage.setItem('pa_token','x'); localStorage.setItem('pa_current','s-445270d3'); true`)
    // reload to re-boot with a current session
    await ws.send('Page.enable')
    await ws.send('Page.reload', { ignoreCache: true })
    await sleep(3000)
    out.seatText2 = await evalJS(`(document.querySelector('#model-seat-label') || {}).textContent`)
    out.seatDisabled2 = await evalJS(`(document.querySelector('#model-seat') || {}).disabled`)
    // Force-unlock then click
    await evalJS(`(function(){ const b=document.querySelector('#model-seat'); if(b){ b.disabled=false; b.click(); } return !!b })()`)
    await sleep(400)
    out.menuHidden = await evalJS(`document.querySelector('#model-menu').classList.contains('hidden')`)
    out.menuHTML = await evalJS(`document.querySelector('#model-menu').innerHTML`)
    out.menuItemCount = await evalJS(`document.querySelectorAll('#model-menu .hm-item').length`)
    out.menuText = await evalJS(`document.querySelector('#model-menu').textContent`)
    console.log('RESULT=' + JSON.stringify(out, null, 2))
    ws.close()
  } finally { chrome.kill() }
}
main().catch((e) => { console.error('FAIL', e); process.exit(1) })
