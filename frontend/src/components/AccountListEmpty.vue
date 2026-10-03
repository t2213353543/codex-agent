<script setup>
import { User, Warning } from '@element-plus/icons-vue'
defineProps({ loading: Boolean, error: String, filtered: Boolean, busy: Boolean })
defineEmits(['retry', 'clear', 'import'])
</script>

<template>
  <div class="accounts-empty" role="status" aria-live="polite">
    <template v-if="loading"><strong>正在加载账号…</strong><span>正在读取账号与资源配置</span></template>
    <template v-else-if="error"><el-icon :size="26"><Warning /></el-icon><strong>账号暂时无法加载</strong><span>重试后可继续查看账号；已有配置不会受影响。</span><el-button @click="$emit('retry')">重新加载</el-button></template>
    <template v-else><el-icon :size="26"><User /></el-icon><strong>{{ filtered ? '没有匹配的账号' : '还没有导入账号' }}</strong><span>{{ filtered ? '调整筛选条件，或清除筛选查看全部账号。' : '导入登录凭据后，即可在这里维护账号和用量。' }}</span><el-button v-if="filtered" @click="$emit('clear')">清除筛选</el-button><el-button v-else type="primary" :disabled="busy" @click="$emit('import')">开始导入</el-button></template>
  </div>
</template>

<style scoped>
.accounts-empty { display: flex; min-height: 190px; flex-direction: column; align-items: center; justify-content: center; gap: 9px; padding: 28px 18px; color: var(--text-3); line-height: 1.6; text-align: center; }
.accounts-empty .el-icon { color: var(--accent-blue); }
.accounts-empty strong { color: var(--text-1); font-size: 14px; font-weight: 600; }
.accounts-empty span { font-size: 12px; }
</style>
