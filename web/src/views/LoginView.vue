<script setup>
import { onMounted, ref } from 'vue'
import { api } from '@/api'

const emit = defineEmits(['signed-in'])

const props = defineProps({
  // Empty when the server runs without a Turnstile pair; then the form is
  // password-only, exactly as it was.
  siteKey: { type: String, default: '' },
})

const username = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

const widgetHost = ref(null)
const token = ref('')
const widgetError = ref('')
let widgetId = null

function loadScript() {
  return new Promise((resolve, reject) => {
    if (window.turnstile) {
      resolve()
      return
    }
    const s = document.createElement('script')
    s.src = 'https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit'
    s.async = true
    s.defer = true
    s.onload = resolve
    s.onerror = () => reject(new Error('script failed'))
    document.head.appendChild(s)
  })
}

onMounted(async () => {
  if (!props.siteKey) return
  try {
    await loadScript()
    widgetId = window.turnstile.render(widgetHost.value, {
      sitekey: props.siteKey,
      theme: 'auto',
      callback: (t) => { token.value = t },
      'expired-callback': () => { token.value = '' },
      'error-callback': () => { token.value = '' },
    })
  } catch {
    // The form stays usable; the server will refuse the login and explain.
    widgetError.value = '人机验证组件加载失败，请检查网络后刷新'
  }
})

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    if (props.siteKey && !token.value) {
      throw Object.assign(new Error('请先完成人机验证'), { handled: true })
    }
    emit('signed-in', await api.login(username.value.trim(), password.value, token.value))
  } catch (e) {
    // 429 carries the remaining wait, so the operator knows when to try again.
    error.value = e.message || '登录失败'
    password.value = ''
    // A spent token cannot answer twice; draw a fresh challenge.
    if (window.turnstile && widgetId !== null) {
      window.turnstile.reset(widgetId)
      token.value = ''
    }
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="login-shell">
    <form class="card login-card" @submit.prevent="submit">
      <h1>QQ 群机器人后台</h1>
      <p class="sub">配置 @机器人 时调用的模型</p>

      <p v-if="error" class="msg err">{{ error }}</p>

      <label>
        <span>用户名</span>
        <input v-model="username" type="text" autocomplete="username" autofocus required>
      </label>
      <label>
        <span>密码</span>
        <input v-model="password" type="password" autocomplete="current-password" required>
      </label>

      <div v-if="siteKey" class="turnstile-slot">
        <div ref="widgetHost"></div>
        <p v-if="widgetError" class="msg err">{{ widgetError }}</p>
      </div>

      <button type="submit" :disabled="busy">{{ busy ? '登录中…' : '登录' }}</button>
    </form>
  </div>
</template>
