<script setup>
import { onMounted, ref } from 'vue'
import { api, setCSRF } from '@/api'
import AccountDialog from '@/components/AccountDialog.vue'
import LoginView from '@/views/LoginView.vue'
import ModelsView from '@/views/ModelsView.vue'

// Three states only: the first render waits for the session check, so the app
// never flashes the model list at a logged-out operator.
const state = ref('loading')
const username = ref('')
const accountOpen = ref(false)
const turnstileKey = ref('')

function apply(session) {
  if (session && session.authenticated) {
    setCSRF(session.csrf)
    username.value = session.username
    state.value = 'authed'
  } else {
    setCSRF('')
    username.value = ''
    state.value = 'anonymous'
  }
  // The site key arrives with every session probe; manual sign-outs pass no
  // session object, and the key the login page needs must survive those.
  if (session && session.turnstile_site_key !== undefined) {
    turnstileKey.value = session.turnstile_site_key
  }
}

function signOut() {
  accountOpen.value = false
  apply({ authenticated: false })
  api.logout().catch(() => {})
}

// The account dialog signs the operator out server-side; the login form is
// where the new credentials are proven.
function accountUpdated() {
  accountOpen.value = false
  apply({ authenticated: false })
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

  <LoginView v-else-if="state === 'anonymous'" :site-key="turnstileKey" @signed-in="apply" />

  <div v-else>
    <header class="topbar">
      <div class="brand">QQ 群<span>机器人</span>后台</div>
      <nav>
        <span>{{ username }}</span>
        <button class="plain small" @click="accountOpen = true">账号</button>
        <button class="plain small" @click="signOut">退出</button>
      </nav>
    </header>

    <main class="wrap">
      <ModelsView @signed-out="apply({ authenticated: false })" />
    </main>

    <AccountDialog
      v-if="accountOpen"
      :username="username"
      @close="accountOpen = false"
      @updated="accountUpdated"
    />
  </div>
</template>
