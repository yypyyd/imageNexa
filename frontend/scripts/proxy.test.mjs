import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import dgram from 'node:dgram'
import { once } from 'node:events'
import { chmod, mkdtemp, readFile, writeFile, mkdir, rm } from 'node:fs/promises'
import http from 'node:http'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import test from 'node:test'

const nginx = process.env.NGINX_BINARY || 'nginx'

async function listen(server, host, port = 0) {
  server.listen(port, host)
  await once(server, 'listening')
  return server.address().port
}

async function eventually(check, timeout = 15000) {
  const deadline = Date.now() + timeout
  let lastError
  while (Date.now() < deadline) {
    try { return await check() } catch (error) { lastError = error }
    await delay(200)
  }
  throw lastError
}

test('nginx preserves API paths and recovers after the backend DNS address changes', { timeout: 55000 }, async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'nexa-proxy-test-'))
  // A root-started Unix nginx drops worker privileges; its static root must be
  // traversable even though mkdtemp intentionally creates a private directory.
  if (process.platform !== 'win32') await chmod(directory, 0o755)
  const normalized = directory.replaceAll('\\', '/')
  const dns = dgram.createSocket('udp4')
  let address = null
  const backends = []
  let child
  let childError
  let output = ''

  t.after(async () => {
    if (child && child.exitCode === null && !childError) {
      const quit = spawn(nginx, ['-p', `${normalized}/`, '-c', 'nginx.conf', '-s', 'quit'], { windowsHide: true })
      quit.on('error', () => {})
      await Promise.race([once(quit, 'exit').catch(() => {}), delay(3000)])
      if (quit.exitCode === null) quit.kill()
      await Promise.race([once(child, 'exit').catch(() => {}), delay(3000)])
      if (child.exitCode === null) child.kill()
    }
    for (const server of backends) {
      server.closeAllConnections()
      if (server.listening) await new Promise((resolve) => server.close(resolve))
    }
    dns.close()
    assert.equal(path.dirname(directory), path.resolve(os.tmpdir()))
    assert.ok(path.basename(directory).startsWith('nexa-proxy-test-'))
    await rm(directory, { recursive: true, force: true })
  })

  dns.on('message', (query, peer) => {
    let end = 12
    while (query[end]) end += query[end] + 1
    end += 5
    const question = query.subarray(12, end)
    const header = Buffer.alloc(12)
    query.copy(header, 0, 0, 2)
    header.writeUInt16BE(address ? 0x8180 : 0x8183, 2)
    header.writeUInt16BE(1, 4)
    header.writeUInt16BE(address ? 1 : 0, 6)
    const answer = address
      ? Buffer.from([0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, ...address.split('.').map(Number)])
      : Buffer.alloc(0)
    dns.send(Buffer.concat([header, question, answer]), peer.port, peer.address)
  })
  dns.bind(0, '127.0.0.1')
  await once(dns, 'listening')

  function backend(name) {
    const server = http.createServer((request, response) => {
      response.setHeader('Content-Type', 'application/json')
      response.end(JSON.stringify({ name, uri: request.url, authorization: request.headers.authorization }))
    })
    backends.push(server)
    return server
  }
  const first = backend('first')
  const backendPort = await listen(first, '127.0.0.1')
  await listen(backend('replacement'), '127.0.0.2', backendPort)
  const portProbe = net.createServer()
  const port = await listen(portProbe, '127.0.0.1')
  await new Promise((resolve) => portProbe.close(resolve))

  await mkdir(path.join(directory, 'logs'))
  await mkdir(path.join(directory, 'temp'))
  await writeFile(path.join(directory, 'index.html'), 'ready')
  const template = await readFile(new URL('../default.conf.template', import.meta.url), 'utf8')
  const serverConfig = template
    .replace('listen 2000;', `listen ${port};`)
    .replace('listen [::]:2000;', '')
    .replace('root /usr/share/nginx/html;', `root "${normalized}";`)
    .replace('127.0.0.11', `127.0.0.1:${dns.address().port}`)
    .replaceAll('backend:6666', `backend:${backendPort}`)
  await writeFile(path.join(directory, 'nginx.conf'), `
daemon off;
worker_processes 1;
pid "${normalized}/nginx.pid";
error_log "${normalized}/error.log" info;
events { worker_connections 64; }
http {
  access_log off;
  client_body_temp_path "${normalized}/client_temp";
  proxy_temp_path "${normalized}/proxy_temp";
  ${serverConfig}
}
`)
  child = spawn(nginx, ['-p', `${normalized}/`, '-c', 'nginx.conf'], { windowsHide: true })
  child.on('error', (error) => { childError = error })
  child.stdout.on('data', (data) => { output += data })
  child.stderr.on('data', (data) => { output += data })
  const base = `http://127.0.0.1:${port}`
  await eventually(async () => {
    if (childError) throw childError
    assert.equal(child.exitCode, null, output)
    const response = await fetch(base, { signal: AbortSignal.timeout(1000) })
    assert.equal(response.status, 200)
  })
  const pid = await readFile(path.join(directory, 'nginx.pid'), 'utf8')
  // Startup must survive a temporary gap in Docker service discovery.
  assert.equal((await fetch(`${base}/health/live`)).status, 502)
  address = '127.0.0.1'
  const paths = ['/health/live', '/health/ready', '/admin/api/accounts?limit=20&q=a%2Bb', '/v1/images/tasks?request_id=a%2Fb&limit=1']
  async function verify(name) {
    await eventually(async () => {
      const response = await fetch(`${base}/health/live`, { signal: AbortSignal.timeout(1000) })
      assert.equal(response.status, 200)
      assert.equal((await response.json()).name, name)
    })
    for (const uri of paths) {
      const response = await fetch(`${base}${uri}`, { headers: { Authorization: 'Bearer test-only-key' } })
      assert.equal(response.status, 200)
      assert.equal(response.headers.get('cache-control'), 'no-store')
      assert.deepEqual(await response.json(), { name, uri, authorization: 'Bearer test-only-key' })
    }
  }
  await verify('first')
  address = '127.0.0.2'
  first.closeAllConnections()
  await new Promise((resolve) => first.close(resolve))
  await verify('replacement')
  assert.equal(await readFile(path.join(directory, 'nginx.pid'), 'utf8'), pid, 'recovery must not restart nginx')
})
