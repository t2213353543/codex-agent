<script setup>
defineProps({ filtered: Boolean, kind: String, hint: String, action: String, disabled: Boolean, loading: Boolean, error: String })
defineEmits(['create', 'reset', 'retry'])
</script>

<template>
  <div class="resource-empty" role="status">
    <span class="resource-empty-icon"><el-icon><Search v-if="filtered" /><Collection v-else /></el-icon></span>
    <strong>{{ loading ? `正在加载${kind}…` : error ? `读取${kind}失败` : filtered ? `没有符合筛选条件的${kind}` : `还没有${kind}` }}</strong>
    <p>{{ loading ? '正在读取最新数据，请稍候。' : error ? '暂时无法取得数据，请稍后重试。' : filtered ? '试试其他关键词，或重置筛选查看全部资源。' : hint }}</p>
    <el-button v-if="!loading" :type="filtered || error ? 'default' : 'primary'" :disabled="disabled" @click="$emit(error ? 'retry' : filtered ? 'reset' : 'create')">{{ error ? '重新加载' : filtered ? '重置筛选' : action }}</el-button>
  </div>
</template>

<style scoped>
.resource-empty { display: flex; flex-direction: column; align-items: center; gap: 10px; padding: 34px 20px; line-height: 1.6; white-space: normal; }
.resource-empty-icon { display: grid; place-items: center; width: 42px; height: 42px; margin-bottom: 2px; border-radius: 12px; color: var(--primary); background: var(--primary-soft); font-size: 20px; }
.resource-empty strong { color: var(--text-1); font-size: 14px; font-weight: 600; }
.resource-empty p { margin: 0 0 6px; color: var(--text-3); font-size: 12px; }
</style>
