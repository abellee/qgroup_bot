<script setup>
import { ref } from 'vue'
import { api } from '@/api'

// The dialog replays the exact call a group @ makes against one stored row -
// enabled or not - so a credential, a base url or a persona is checked before
// anything reaches the group. No history is kept here either: every send is a
// fresh single turn, like the reply path.
const props = defineProps({
  row: { type: Object, required: true },
})
const emit = defineEmits(['close'])

const prompt = ref('')
const answer = ref('')
const took = ref(null)
const busy = ref(false)
const error = ref('')

async function send() {
  if (busy.value) return
  const text = prompt.value.trim()
  if (!text) return
  busy.value = true
  error.value = ''
  answer.value = ''
  took.value = null
  try {
    const out = await api.testModel(props.row.id, text)
    answer.value = out.answer || '（模型返回了空回复）'
    took.value = out.took_ms
  } catch (e) {
    error.value = e.message || '请求失败'
  } finally {
    busy.value = false
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
        <h2>测试「{{ row.name }}」</h2>
        <button class="plain small close" @click="emit('close')">关闭</button>
      </div>

      <p class="hint">
        按 {{ row.provider }} · {{ row.model }} 直接调用一次，回答只在页面显示，不进群。
        {{ row.enabled ? '' : '这条配置还没启用，通过测试再启用也不迟。' }}
      </p>

      <label>
        <span>发送内容</span>
        <textarea
          v-model="prompt"
          placeholder="写一句想发到群里 @机器人 的话，Ctrl+Enter 发送"
          @keydown.ctrl.enter.prevent="send"
        ></textarea>
      </label>

      <p v-if="error" class="msg err">{{ error }}</p>

      <div v-if="answer !== ''" class="answer">
        <div class="answer-meta">
          回答<template v-if="took !== null"> · {{ (took / 1000).toFixed(1) }}s</template>
        </div>
        <div class="answer-body">{{ answer }}</div>
      </div>

      <div class="sheet-foot">
        <button :disabled="busy || !prompt.trim()" @click="send">{{ busy ? '调用中…' : '发送' }}</button>
        <button type="button" class="plain" @click="emit('close')">关闭</button>
      </div>
    </section>
  </div>
</template>
