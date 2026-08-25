// Minimal CDP driver: launch chrome, open the page, click the model seat,
// read the rendered model menu HTML. Uses only node built-ins (http + ws via
// raw socket is complex; instead we drive through chrome's --remote-debugging
// and the JSON HTTP endpoint + a tiny WebSocket client).
import { spawn } from 'node:child_process'
import http from 'node:http'

const CHROME = process.env.CHROME
const PROFILE = process.env.PROFILE
const PAGE = process.env.PAGE
const PORT = 9229

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

// Minimal WebSocket client (no external deps).
class WS {
  constructor(url) {
    this.url = url
    this.id = 0
    this.pending = new Map()
    this.events = new Map()
  }
  connect() {
    return new Promise((resolve, reject) => {
      const u = new URL(this.url)
      const key = Buffer.from(Math.random().toString(36)).toString('base64')
      const req = http.request({
        host: u.hostname, port: u.port, path: u.pathname + u.search,
        headers: {
          Connection: 'Upgrade', Upgrade: 'websocket',
          'Sec-WebSocket-Key': key, 'Sec-WebSocket-Version': '13',
        },
      })
      req.on('upgrade', (res, socket) => {
        this.socket = socket
        socket.on('data', (buf) => this._onData(buf))
        socket.on('error', () => {})
        resolve()
      })
      req.on('error', reject)
      req.end()
    })
  }
  _onData(buf) {
    // WebSocket frames are masked by client; server frames unmasked. We only
    // handle text frames (opcode 0x81) and ignore everything else. Buffer may
    // contain partial frames; keep a remainder.
    this._remainder = this._remainder ? Buffer.concat([this._remainder, buf]) : buf
    while (this._remainder.length >= 2) {
      const b0 = this._remainder[0]
      const b1 = this._remainder[1]
      const opcode = b0 & 0x0f
      const len = b1 & 0x7f
      let offset = 2
      let payloadLen = len
      if (len === 126) {
        if (this._remainder.length < 4) return
        payloadLen = this._remainder.readUInt16BE(2)
        offset = 4
      } else if (len === 127) {
        if (this._remainder.length < 10) return
        payloadLen = Number(this._remainder.readBigUInt64BE(2))
        offset = 10
      }
      if (this._remainder.length < offset + payloadLen) return
      const payload = this._remainder.subarray(offset, offset + payloadLen)
      this._remainder = this._remainder.subarray(offset + payloadLen)
      if (opcode === 1) {
        const msg = JSON.parse(payload.toString('utf8'))
        if (msg.id !== undefined) {
          const p = this.pending.get(msg.id)
          if (p) { this.pending.delete(msg.id); p.resolve(msg) }
        } else if (msg.method) {
          const list = this.events.get(msg.method)
          if (list) for (const fn of list) fn(msg.params)
        }
      } else if (opcode === 8) {
        // close
        this._remainder = Buffer.alloc(0)
      }
    }
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      const payload = JSON.stringify({ id, method, params })
      // client frames must be masked: 0x81 | 0x80, 4-byte mask
      const body = Buffer.from(payload, 'utf8')
      let header
      const mask = Buffer.from([1, 2, 3, 4])
      if (body.length < 126) {
        header = Buffer.from([0x81, 0x80 | body.length])
      } else if (body.length < 65536) {
        header = Buffer.alloc(4)
        header[0] = 0x81; header[1] = 0x80 | 126
        header.writeUInt16BE(body.length, 2)
      } else {
        header = Buffer.alloc(10)
        header[0] = 0x81; header[1] = 0x80 | 127
        header.writeBigUInt64BE(BigInt(body.length), 2)
      }
      const masked = Buffer.alloc(body.length)
      for (let i = 0; i < body.length; i++) masked[i] = body[i] ^ mask[i % 4]
      this.socket.write(Buffer.concat([header, mask, masked]))
    })
  }
  on(method, fn) {
    if (!this.events.has(method)) this.events.set(method, [])
    this.events.get(method).push(fn)
  }
  close() { try { this.socket.end() } catch {} }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(fn, timeout = 10000) {
  const start = Date.now()
  while (Date.now() - start < timeout) {
    try {
      const v = await fn()
      if (v) return v
    } catch {}
    await sleep(100)
  }
  throw new Error('timeout waiting')
}

async function main() {
  const chrome = spawn(CHROME, [
    '--headless=new', '--disable-gpu', '--no-first-run',
    '--remote-debugging-port=' + PORT,
    `--user-data-dir=${PROFILE}`,
    PAGE,
  ], { stdio: 'ignore' })

  try {
    const target = await waitFor(async () => {
      const list = await getJSON(`http://127.0.0.1:${PORT}/json`)
      return list.find((t) => t.type === 'page')
    }, 15000)

    const ws = new WS(target.webSocketDebuggerUrl)
    await ws.connect()

    const send = (m, p) => ws.send(m, p)
    await send('Runtime.enable')

    // wait for page load + boot
    await sleep(2500)

    const evalJS = async (expr) => {
      const res = await send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true })
      if (res.result && res.result.exceptionDetails) {
        throw new Error('eval exception: ' + JSON.stringify(res.result.exceptionDetails))
      }
      return res.result ? res.result.result.value : undefined
    }

    const out = {}
    out.hasToken = await evalJS(`!!localStorage.getItem('pa_token')`)
    out.configKeys = await evalJS(`Object.keys(window.config || {})`)
    out.providerCount = await evalJS(`(window.config && window.config.providers) ? window.config.providers.length : -1`)
    out.seatText = await evalJS(`(document.querySelector('#model-seat-label') || {}).textContent`)

    // click the model seat
    await evalJS(`(function(){ const b = document.querySelector('#model-seat'); if (b) b.click(); return !!b })()`)
    await sleep(300)

    out.menuHidden = await evalJS(`document.querySelector('#model-menu').classList.contains('hidden')`)
    out.menuHTML = await evalJS(`document.querySelector('#model-menu').innerHTML`)
    out.menuItemCount = await evalJS(`document.querySelectorAll('#model-menu .hm-item').length`)

    console.log('RESULT=' + JSON.stringify(out, null, 2))
    ws.close()
  } finally {
    chrome.kill()
  }
}

main().catch((e) => { console.error('FAIL', e); process.exit(1) })
