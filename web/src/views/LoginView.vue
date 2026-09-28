<script setup>
import { ref } from 'vue'
import { api } from '@/api'

const emit = defineEmits(['signed-in'])

const username = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    emit('signed-in', await api.login(username.value.trim(), password.value))
  } catch (e) {
    // 429 carries the remaining wait, so the operator knows when to try again.
    error.value = e.message || '登录失败'
  } finally {
    busy.value = false
    password.value = ''
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

      <button type="submit" :disabled="busy">{{ busy ? '登录中…' : '登录' }}</button>
    </form>
  </div>
</template>
