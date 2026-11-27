import assert from 'node:assert/strict'
import { lstatSync, readFileSync, readdirSync } from 'node:fs'

const modules = JSON.parse(readFileSync('vite.build.json', 'utf8')).Test
const release = process.env.G2VW_RELEASE
assert.match(release, /^\d+-\d+$/)
assert.ok(modules.length, 'Empty build module list')
assert.deepEqual(readdirSync('dist/module').sort(), [...modules].sort())

for (const name of modules) {
  assert.match(name, /^[A-Za-z0-9_-]+$/)
  const dir = 'dist/module/' + name
  assert.deepEqual(readdirSync(dir), ['index.html'])
  const html = readFileSync(dir + '/index.html', 'utf8')
  const urls = [...html.matchAll(/(?:src|href)=["']([^"']+)["']/g)].map(match => match[1])
  assert.ok(urls.includes('/static/config.js'), 'Missing shared runtime config in ' + name)
  const local = urls.filter(url => url.startsWith('/script/') || url.startsWith('/assets/'))
  assert.ok(local.some(url => url.endsWith('.js')), 'Missing entry JS in ' + name)
  for (const url of local) {
    assert.ok(url.startsWith('/script/g2vw/' + release + '/') || url.startsWith('/assets/g2vw/' + release + '/'), url)
    assert.ok(lstatSync('dist' + url).isFile(), url)
  }
}
