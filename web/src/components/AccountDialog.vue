<script setup>
import { ref } from 'vue'
import { api } from '@/api'

// Changing anything requires the old password, and the server kills every
// session with the change - this one included - so success signs the operator
// out and the login form takes over.
const props = defineProps({
  username: { type: String, required: true },
})
const emit = defineEmits(['close', 'updated'])

const username = ref(props.username)
const oldPassword = ref('')
const newPassword = ref('')
const confirm = ref('')
const busy = ref(false)
const error = ref('')

async function submit() {
  if (busy.value) return
  error.value = ''
  if (!oldPassword.value) {
    error.value = '先输入旧密码'
    return
  }
  if (newPassword.value && newPassword.value !== confirm.value) {
    error.value = '两次输入的新密码不一致'
    return
  }
  busy.value = true
  try {
    await api.updateAccount({
      old_password: oldPassword.value,
      username: username.value.trim(),
      new_password: newPassword.value,
    })
    emit('updated')
  } catch (e) {
    error.value = e.message || '保存失败'
  } finally {
    busy.value = false
    oldPassword.value = ''
    newPassword.value = ''
    confirm.value = ''
  }
}

function onKeydown(e) {
  if (e.key === 'Escape') emit('close')
}
</script>

<template>
  <div class="scrim" @keydown="onKeydown">
    <section class="sheet" role="dialog" aria-modal="true">
      <div class="sheet-head">
        <h2>管理员账号</h2>
        <button class="plain small close" @click="emit('close')">关闭</button>
      </div>

      <form @submit.prevent="submit">
        <p class="hint">用户名和新密码想改哪个填哪个；保存成功后所有登录都会失效，要用新账号重新登录。</p>

        <label>
          <span>用户名</span>
          <input v-model="username" type="text" autocomplete="username">
        </label>
        <label>
          <span>旧密码（必填）</span>
          <input v-model="oldPassword" type="password" autocomplete="current-password">
        </label>
        <label>
          <span>新密码（留空则不修改）</span>
          <input v-model="newPassword" type="password" autocomplete="new-password">
          <span class="hint">超过 72 字节会被拒绝。</span>
        </label>
        <label>
          <span>确认新密码</span>
          <input v-model="confirm" type="password" autocomplete="new-password">
        </label>

        <p v-if="error" class="msg err">{{ error }}</p>

        <div class="sheet-foot">
          <button type="submit" :disabled="busy">{{ busy ? '保存中…' : '保存' }}</button>
          <button type="button" class="plain" @click="emit('close')">取消</button>
        </div>
      </form>
    </section>
  </div>
</template>
