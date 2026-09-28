<script setup>
import { computed, onMounted, ref } from 'vue'
import { api } from '@/api'
import ModelEditor from '@/components/ModelEditor.vue'

const emit = defineEmits(['signed-out'])

const rows = ref([])
const providers = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')
const editing = ref(null)
const sheetOpen = ref(false)

const active = computed(() => rows.value.find((m) => m.enabled) || null)

let noticeTimer = null
function say(text, kind = 'ok') {
  notice.value = kind === 'ok' ? text : ''
  if (kind !== 'ok') {
    error.value = text
    return
  }
  clearTimeout(noticeTimer)
  noticeTimer = setTimeout(() => { notice.value = '' }, 2600)
}

// A 401 from any call means the 12-hour session went away while the page was
// open, which is the only case worth bouncing to the login form for.
function unwrap(e) {
  if (e && e.status === 401) {
    emit('signed-out')
    return '登录已过期，请重新登录'
  }
  return (e && e.message) || '请求失败'
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [m, p] = await Promise.all([api.models(), api.providers()])
    rows.value = m.models || []
    providers.value = p.providers || []
  } catch (e) {
    error.value = unwrap(e)
  } finally {
    loading.value = false
  }
}

onMounted(load)

function openNew() {
  editing.value = null
  sheetOpen.value = true
}
function openEdit(row) {
  editing.value = row
  sheetOpen.value = true
}

async function save(payload) {
  try {
    await api.saveModel(payload)
    sheetOpen.value = false
    await load()
    say('已保存')
  } catch (e) {
    // The server names the missing fields; keep them visible in the sheet.
    say(unwrap(e), 'err')
    return false
  }
  return true
}

async function enable(row) {
  try {
    await api.enableModel(row.id)
    await load()
    say(`已启用「${row.name}」，@机器人 的回复立刻走这条配置`)
  } catch (e) {
    say(unwrap(e), 'err')
  }
}

async function remove(row) {
  if (!window.confirm(`删除「${row.name}」？`)) return
  try {
    await api.deleteModel(row.id)
    if (editing.value && editing.value.id === row.id) sheetOpen.value = false
    await load()
    say('已删除')
  } catch (e) {
    say(unwrap(e), 'err')
  }
}

function time(text) {
  if (!text) return '—'
  const d = new Date(text)
  return Number.isNaN(d.getTime()) ? text : d.toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <div class="page-head">
    <h1>模型</h1>
    <div class="spacer"></div>
    <button class="plain" :disabled="loading" @click="load">刷新</button>
    <button :disabled="loading" @click="openNew">新增模型配置</button>
  </div>

  <p v-if="error" class="msg err">{{ error }}</p>

  <div v-if="!loading && !rows.length" class="card empty">
    <b>还没有模型配置</b>
    <p>新增一条并勾选启用后，群里 @机器人 的消息就会发给这个模型，回答贴回群里。<br>没有任何配置时机器人保持安静，入群审批不受影响。</p>
    <button @click="openNew">新增第一条</button>
  </div>

  <template v-else>
    <div class="card status-strip">
      <template v-if="active">
        <span class="badge on">启用中</span>
        <b>{{ active.name }}</b>
        <span class="badge">{{ active.provider }}</span>
        <span>{{ active.model }}</span>
        <span class="row-meta">· {{ active.base_url }}</span>
      </template>
      <template v-else>
        <span class="badge warn">未启用</span>
        <span>当前没有生效的配置，@机器人 不会回话</span>
      </template>
    </div>

    <ul class="rows card">
      <li v-for="row in rows" :key="row.id">
        <div class="row-main">
          <div class="row-title">
            <span class="name">{{ row.name }}</span>
            <span class="badge">{{ row.provider }}</span>
            <span v-if="row.enabled" class="badge on">启用中</span>
          </div>
          <div class="row-sub">{{ row.model }} · {{ row.base_url }}</div>
          <div class="row-meta">
            Key {{ row.key_masked }}
            <template v-if="row.persona"> · 人设 {{ row.persona.length > 24 ? row.persona.slice(0, 24) + '…' : row.persona }}</template>
          </div>
        </div>

        <div class="row-meta">
          <div>温度 {{ row.temperature }}</div>
          <div>max tokens {{ row.max_tokens }}</div>
          <div>超时 {{ row.timeout_ms }} ms</div>
          <div>{{ time(row.updated_at) }}</div>
        </div>

        <div class="row-ops">
          <button v-if="!row.enabled" class="small" @click="enable(row)">启用</button>
          <button class="plain small" @click="openEdit(row)">编辑</button>
          <button class="danger small" @click="remove(row)">删除</button>
        </div>
      </li>
    </ul>
  </template>

  <ModelEditor
    v-if="sheetOpen"
    :row="editing"
    :providers="providers"
    @close="sheetOpen = false"
    @save="save"
  />

  <div v-if="notice" class="toast">{{ notice }}</div>
</template>
