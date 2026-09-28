<script setup>
import { computed, reactive, ref } from 'vue'
import { api } from '@/api'

const props = defineProps({
  // null means a new row; anything else is the stored row being edited, whose
  // API key the panel can only ever show masked.
  row: { type: Object, default: null },
  providers: { type: Array, default: () => [] },
  // The save call as a function, not an event: emit cannot carry a promise
  // back, and the button has to stay held until the panel has answered.
  save: { type: Function, required: true },
})
const emit = defineEmits(['close'])

const form = reactive({
  id: props.row ? props.row.id : 0,
  name: props.row ? props.row.name : '',
  provider: props.row ? props.row.provider : (props.providers[0] && props.providers[0].value) || 'openai',
  base_url: props.row ? props.row.base_url : '',
  api_key: '',
  model: props.row ? props.row.model : '',
  persona: props.row ? props.row.persona : '',
  fallback_replies: props.row ? props.row.fallback_replies || '' : '',
  temperature: props.row ? props.row.temperature : 1,
  max_tokens: props.row ? props.row.max_tokens : 1024,
  timeout_ms: props.row ? props.row.timeout_ms : 45000,
  enabled: props.row ? props.row.enabled : false,
})

const busy = ref(false)
const keyGiven = computed(() => !!(props.row && props.row.key_given))

// The model catalog the fetch button pulls: the request carries the form's own
// provider and base url, and a blank key means the server asks the provider
// with the stored one, exactly the semantics a save applies.
const remoteLoading = ref(false)
const remoteModels = ref([])
const remoteError = ref('')
const pickedModel = ref('')

async function pullModels() {
  if (remoteLoading.value) return
  remoteLoading.value = true
  remoteError.value = ''
  remoteModels.value = []
  pickedModel.value = ''
  try {
    const out = await api.remoteModels({
      id: form.id,
      provider: form.provider,
      base_url: form.base_url,
      api_key: form.api_key,
    })
    remoteModels.value = out.models || []
    if (!remoteModels.value.length) remoteError.value = '上游没有返回任何模型'
  } catch (e) {
    remoteError.value = e.message || '拉取失败'
  } finally {
    remoteLoading.value = false
  }
}

// A real select instead of a datalist: datalist entries get filtered by the text
// already in the input, and the operator wants to see everything the provider
// offers, unchanged.
function pickModel(e) {
  pickedModel.value = e.target.value
  form.model = e.target.value
}

// What the request path will look like for the chosen provider, so a wrong base
// url is visible before it silently fails in the group.
const protocolHint = computed(() => {
  const base = form.base_url.trim().replace(/\/+$/, '') || 'https://your-api-host'
  if (form.provider === 'openai-responses') return `${base}/v1/responses · 头 Authorization: Bearer · 不发送温度`
  if (form.provider === 'anthropic') return `${base}/v1/messages · 头 x-api-key`
  if (form.provider === 'gemini') return `${base}/v1beta/models/${form.model || '模型名'}:generateContent · 头 x-goog-api-key`
  return `${base}/v1/chat/completions · 头 Authorization: Bearer`
})

async function submit() {
  if (busy.value) return
  busy.value = true
  // A false return leaves the sheet open with the message shown, so nothing the
  // operator typed is lost.
  try {
    await props.save({ ...form })
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
        <h2>{{ form.id ? '编辑模型配置' : '新增模型配置' }}</h2>
        <button class="plain small close" @click="emit('close')">关闭</button>
      </div>

      <form @submit.prevent="submit">
        <label>
          <span>名称</span>
          <input v-model="form.name" type="text" placeholder="主用 / 备用测试，列表里认得出来就行">
        </label>

        <label>
          <span>提供商</span>
          <select v-model="form.provider">
            <option v-for="p in providers" :key="p.value" :value="p.value">{{ p.label }}</option>
          </select>
          <span class="hint">{{ protocolHint }}</span>
        </label>

        <label>
          <span>API 地址</span>
          <input v-model="form.base_url" type="text" placeholder="https://api.openai.com/v1">
          <span class="hint">填到版本段为止；不带 /v1（/v1beta）时程序会自动补上。</span>
        </label>

        <label>
          <span>API Key</span>
          <input v-model="form.api_key" type="text" :placeholder="keyGiven ? `已保存 ${row.key_masked}，留空则不修改` : '填完整密钥'">
          <span class="hint">只存在服务器上的 SQLite 里，页面只显示掩码。</span>
        </label>

        <label>
          <span>模型</span>
          <div class="with-btn">
            <input v-model="form.model" type="text" placeholder="gpt-4o-mini / claude-sonnet-4-5 / gemini-2.5-flash">
            <button type="button" class="plain" :disabled="remoteLoading" @click="pullModels">{{ remoteLoading ? '拉取中…' : '获取列表' }}</button>
          </div>
          <select v-if="remoteModels.length" class="model-pick" :value="pickedModel" @change="pickModel">
            <option value="" disabled>从上游的 {{ remoteModels.length }} 个模型里选择…</option>
            <option v-for="m in remoteModels" :key="m" :value="m">{{ m }}</option>
          </select>
          <span v-if="remoteError" class="hint err">{{ remoteError }}</span>
        </label>

        <label>
          <span>人设（system 提示词）</span>
          <textarea v-model="form.persona" placeholder="你是 QQ 群的助手，回答要短、口语化，不要用 markdown 标题。"></textarea>
          <span class="hint">每次 @机器人 只带这一句人设和群里那句话，不记录历史。</span>
        </label>

        <label>
          <span>接口报错时的候选回复（一行一条）</span>
          <textarea v-model="form.fallback_replies" placeholder="模型临时开小差了，稍后再 @ 我试试"></textarea>
          <span class="hint">调用失败或返回为空时，随机挑一条发进群；留空则保持安静。</span>
        </label>

        <div class="grid-3">
          <label>
            <span>温度</span>
            <input v-model.number="form.temperature" type="number" step="0.1" min="0" max="2">
          </label>
          <label>
            <span>最大 tokens</span>
            <input v-model.number="form.max_tokens" type="number" min="1" max="32000">
          </label>
          <label>
            <span>超时（毫秒）</span>
            <input v-model.number="form.timeout_ms" type="number" min="1000" max="300000">
          </label>
        </div>

        <label class="check">
          <input v-model="form.enabled" type="checkbox">
          <span>启用（同一时刻只有这一条生效）</span>
        </label>

        <div class="sheet-foot">
          <button type="submit" :disabled="busy">{{ busy ? '保存中…' : '保存' }}</button>
          <button type="button" class="plain" @click="emit('close')">取消</button>
        </div>
      </form>
    </section>
  </div>
</template>
