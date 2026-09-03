// One-shot repo rename: github.com/shutu-ai/shutu-agent module path -> github.com/shutu-ai/shutu-agent.
// Uses Node fs (UTF-8 strict, no BOM, preserves LF/CRLF as read) so non-ASCII
// comments/docs are never re-encoded by the shell. config.yaml is excluded
// here and handled separately (its User-Agent string takes the bare name).
import fs from 'node:fs'
import path from 'node:path'

const root = process.argv[2] ?? '.'
const TEXT = new Set(['.go', '.mod', '.md', '.html', '.js', '.mjs', '.css', '.yml', '.yaml', '.json', '.toml', '.sum', '.gitignore', '.gitattributes'])

const skipDir = (p) =>
  p.includes(`${path.sep}.gomodcache`) || p.includes(`${path.sep}.gocache`) ||
  p.includes(`${path.sep}.git`) || p.includes(`${path.sep}node_modules`)

let replaced = 0
function walk(dir) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name)
    if (skipDir(p)) continue
    if (e.isDirectory()) walk(p)
    else {
      const ext = path.extname(e.name)
      if (!TEXT.has(ext)) continue
      let c
      try { c = fs.readFileSync(p, 'utf8') } catch { continue }
      if (!c.includes('github.com/shutu-ai/shutu-agent')) continue
      fs.writeFileSync(p, c.replaceAll('github.com/shutu-ai/shutu-agent', 'github.com/shutu-ai/shutu-agent'), 'utf8')
      replaced++
    }
  }
}
walk(root)
console.log('module-path files rewritten:', replaced)
