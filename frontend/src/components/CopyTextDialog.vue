<script setup>
import { nextTick, ref, watch } from 'vue'

const props = defineProps({ modelValue: Boolean, text: { type: String, default: '' }, label: { type: String, default: '待复制的密钥' } })
const emit = defineEmits(['update:modelValue'])
const input = ref(null)
let returnFocus
watch(() => props.modelValue, (visible) => {
  if (visible) returnFocus = document.activeElement
})
function restoreFocus() {
  if (returnFocus?.isConnected) returnFocus.focus()
  returnFocus = undefined
}
async function selectText() {
  await nextTick()
  input.value?.focus()
  input.value?.select()
}
</script>

<template>
  <el-dialog :model-value="modelValue" title="手动复制" class="dialog-secret detail-dialog" width="min(520px, calc(100vw - 24px))" align-center append-to-body :close-on-click-modal="false" @update:model-value="emit('update:modelValue', $event)" @opened="selectText" @closed="restoreFocus">
    <p class="dialog-intro">浏览器未允许访问剪贴板。下方文本已选中，按 Ctrl/Cmd+C 即可复制。</p>
    <el-input ref="input" :model-value="text" type="textarea" :rows="4" readonly :aria-label="label" autocomplete="off" />
    <template #footer><el-button type="primary" @click="emit('update:modelValue', false)">完成</el-button></template>
  </el-dialog>
</template>
