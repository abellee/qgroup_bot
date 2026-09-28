<script setup>
import { onMounted, ref } from 'vue'
import { api, setCSRF } from '@/api'
import LoginView from '@/views/LoginView.vue'
import ModelsView from '@/views/ModelsView.vue'

// Three states only: the first render waits for the session check, so the app
// never flashes the model list at a logged-out operator.
const state = ref('loading')
const username = ref('')

function apply(session) {
  if (session && session.authenticated) {
    setCSRF(session.csrf)
    username.value = session.username
    state.value = 'authed'
    return
  }
  setCSRF('')
  username.value = ''
  state.value = 'anonymous'
}

onMounted(async () => {
  try {
    apply(await api.session())
  } catch {
    apply({ authenticated: false })
  }
})
</script>

<template>
  <div v-if="state === 'loading'" class="skeleton">正在检查登录状态…</div>

  <LoginView v-else-if="state === 'anonymous'" @signed-in="apply" />

  <div v-else>
    <header class="topbar">
      <div class="brand">QQ 群<span>机器人</span>后台</div>
      <nav>
        <span>{{ username }}</span>
        <button class="plain small" @click="apply({ authenticated: false }); api.logout().catch(() => {})">退出</button>
      </nav>
    </header>

    <main class="wrap">
      <ModelsView @signed-out="apply({ authenticated: false })" />
    </main>
  </div>
</template>
