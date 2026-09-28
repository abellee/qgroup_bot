// The panel mounts under a random path only the server knows, so the API base
// follows the page: the app is always served at the mount root, and the
// slashless form of the path redirects there before anything loads.
const base = window.location.pathname.replace(/\/+$/, '') + '/api'

// The csrf token comes from the session endpoint and is echoed on every mutation.
// It lives in a module variable rather than localStorage: the session cookie is
// HttpOnly, so a stolen script still cannot pair a token with a request.
let csrf = ''

export function setCSRF(token) {
  csrf = token || ''
}

async function request(path, { method = 'GET', body } = {}) {
  const headers = {}
  if (body !== undefined) headers['content-type'] = 'application/json'
  if (method !== 'GET' && csrf) headers['x-csrf-token'] = csrf

  const res = await fetch(base + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
  })

  const text = await res.text()
  let data = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      throw new Error(`服务返回了非 JSON 内容（${res.status}）`)
    }
  }

  if (!res.ok) {
    const err = new Error((data && data.error) || `请求失败（${res.status}）`)
    err.status = res.status
    throw err
  }
  return data
}

export const api = {
  session: () => request('/session'),
  login: (username, password, turnstileToken) =>
    request('/login', { method: 'POST', body: { username, password, turnstile_token: turnstileToken || undefined } }),
  logout: () => request('/logout', { method: 'POST', body: {} }),
  providers: () => request('/providers'),
  models: () => request('/models'),
  saveModel: (model) => request('/models', { method: 'POST', body: model }),
  enableModel: (id) => request('/models/enable', { method: 'POST', body: { id } }),
  deleteModel: (id) => request('/models/delete', { method: 'POST', body: { id } }),
  testModel: (id, prompt) => request('/models/test', { method: 'POST', body: { id, prompt } }),
  remoteModels: (payload) => request('/models/remote', { method: 'POST', body: payload }),
  updateAccount: (payload) => request('/account', { method: 'POST', body: payload }),
}
