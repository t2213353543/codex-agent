<template><section class="resource-workspace resource-workspace--comfortable"><slot /></section></template>

<style scoped>
.resource-workspace { min-width: 0; container-type: inline-size; container-name: resources; }
:deep(.resource-panel) { padding: 0; overflow: hidden; }
:deep(.resource-panel-heading) { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 10px; padding: 16px 18px 0; }
:deep(.resource-panel-title) { margin: 0; color: var(--text-1); font-size: 14px; font-weight: 600; }
:deep(.resource-panel-caption) { color: var(--text-3); font-size: 12px; margin: 5px 0 0; line-height: 1.5; }
:deep(.resource-panel .table-toolbar) { gap: 8px; margin: 0; padding: 14px 18px; border-bottom: 1px solid var(--card-border); }
:deep(.resource-panel .table-toolbar .el-input) { flex: 1 1 220px; max-width: 300px !important; }
:deep(.resource-panel .table-toolbar .el-select) { flex: 0 1 140px; }
:deep(.resource-panel .table-toolbar .spacer) { flex: 1 1 8px; }
:deep(.resource-panel .el-table) { --el-table-header-bg-color: var(--el-fill-color-light); font-size: 13px; }
:deep(.resource-panel .el-table th.el-table__cell) { font-size: 12px; font-weight: 500; color: var(--text-2); }
:deep(.resource-panel .el-table .cell) { padding: 0 14px; }
:deep(.resource-name) { appearance: none; display: block; border: none; background: none; padding: 0; margin: 0; text-align: left; font: inherit; color: var(--text-1); font-size: 13px; font-weight: 500; cursor: pointer; overflow-wrap: anywhere; }
:deep(.resource-name:hover) { color: var(--primary); }
:deep(.resource-name:disabled) { cursor: default; }
:deep(.resource-row-sub) { display: block; margin-top: 4px; color: var(--text-3); font-size: 12px; font-weight: 400; line-height: 1.5; }
:deep(.resource-name:focus-visible) { outline: 2px solid var(--primary); outline-offset: 4px; border-radius: 2px; }
:deep(.resource-state) { display: inline-flex; align-items: center; gap: 6px; color: var(--text-3); font-size: 12px; white-space: nowrap; }
:deep(.resource-state::before) { content: ''; width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
:deep(.resource-state.is-enabled) { color: var(--success); }
:deep(.resource-state.is-error) { color: var(--danger); }
:deep(.resource-switch-cell) { display: flex; flex-direction: column; align-items: center; gap: 2px; }
:deep(.resource-switch-cell .resource-state) { font-size: 11px; }
:deep(.resource-inline-code) { display: inline-block; max-width: 100%; padding: 2px 6px; border: 1px solid var(--card-border); border-radius: 5px; background: var(--el-fill-color-light); font-size: 12px; overflow-wrap: anywhere; }
:deep(.resource-protocol) { font-size: 11px; font-weight: 600; letter-spacing: .025em; padding: 2px 6px; border-radius: 4px; background: var(--el-fill-color-light); color: var(--text-2); }
:deep(.resource-selection) { margin: 0; padding: 12px 22px; border-bottom: 1px solid var(--card-border); background: var(--primary-soft); color: var(--primary); font-size: 12px; }
:deep(.resource-table-footer) { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; padding: 16px 22px; }
:deep(.resource-page-size) { display: flex; align-items: center; gap: 8px; color: var(--text-3); font-size: 12px; }
:deep(.resource-page-size .el-select) { width: 82px; }
:deep(.resource-panel-note) { margin: 0; padding: 0 22px 18px; color: var(--text-3); font-size: 11px; line-height: 1.6; }
@media (max-width: 767px) {
  :deep(.resource-desktop-table) { display: none; }
  :deep(.resource-panel-heading) { padding: 16px 14px 0; }
  :deep(.resource-panel .table-toolbar) { padding: 14px; }
  :deep(.resource-panel .table-toolbar .el-input) { flex-basis: 100%; max-width: 100% !important; }
  :deep(.resource-panel .table-toolbar .el-select) { flex: 1 1 100px; }
  :deep(.resource-panel .table-toolbar .spacer) { display: none; }
  :deep(.resource-table-footer) { padding: 14px; justify-content: center; }
  :deep(.resource-page-size) { width: 100%; justify-content: center; }
  :deep(.resource-selection) { padding: 12px 14px; }
  :deep(.resource-panel-note) { padding: 0 14px 16px; }
}
@container resources (max-width: 620px) {
  :deep(.resource-desktop-table) { display: none; }
  :deep(.resource-panel-heading) { padding: 16px 14px 0; }
  :deep(.resource-panel .table-toolbar) { padding: 14px; }
  :deep(.resource-panel .table-toolbar .el-input) { flex-basis: 100%; max-width: 100% !important; }
  :deep(.resource-panel .table-toolbar .el-select) { flex: 1 1 100px; }
  :deep(.resource-panel .table-toolbar .spacer) { display: none; }
  :deep(.resource-table-footer) { padding: 14px; justify-content: center; }
  :deep(.resource-page-size) { width: 100%; justify-content: center; }
  :deep(.resource-selection) { padding: 12px 14px; }
  :deep(.resource-panel-note) { padding: 0 14px 16px; }
}
</style>

<style scoped>
.resource-workspace--comfortable :deep(.resource-panel-heading) { padding:12px 15px 0; gap:8px; }
.resource-workspace--comfortable :deep(.resource-panel-title) { font-size:12px; }
.resource-workspace--comfortable :deep(.resource-panel-caption) { font-size:12px; margin-top:3px; }
.resource-workspace--comfortable :deep(.resource-panel .table-toolbar) { padding:10px 15px; gap:7px; }
.resource-workspace--comfortable :deep(.resource-panel .el-table) { font-size:13px; }
.resource-workspace--comfortable :deep(.resource-panel .el-table th.el-table__cell) { height:32px; font-size:12px; padding:4px 0; }
.resource-workspace--comfortable :deep(.resource-panel .el-table td.el-table__cell) { padding:5px 0; height:42px; }
.resource-workspace--comfortable :deep(.resource-panel .el-table .cell) { padding:0 10px; line-height:1.4; }
.resource-workspace--comfortable :deep(.resource-identity) { display:flex; align-items:center; gap:9px; min-width:0; }
.resource-workspace--comfortable :deep(.resource-identity-copy) { min-width:0; flex:1; }
.resource-workspace--comfortable :deep(.resource-identity-mark) { display:grid; place-items:center; flex:0 0 29px; width:29px; height:29px; border:1px solid color-mix(in srgb,var(--badge-blue,var(--primary)) 13%,transparent); border-radius:7px; color:var(--badge-blue,var(--primary)); background:var(--badge-blue-soft,var(--primary-soft)); }
.resource-workspace--comfortable :deep(.resource-identity-mark.is-purple) { color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); border-color:color-mix(in srgb,var(--badge-purple,var(--primary)) 13%,transparent); }
.resource-workspace--comfortable :deep(.resource-identity-mark.is-amber) { color:var(--badge-amber,var(--warning)); background:var(--badge-amber-soft,var(--primary-soft)); border-color:color-mix(in srgb,var(--badge-amber,var(--warning)) 13%,transparent); }
.resource-workspace--comfortable :deep(.resource-name) { font-size:13px; font-weight:600; line-height:1.5; }
.resource-workspace--comfortable :deep(.resource-row-sub) { font-size:12px; margin-top:2px; line-height:1.5; }
.resource-workspace--comfortable :deep(.resource-id-label) { display:block; max-width:180px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; margin-top:3px; font-size:12px; color:var(--text-3); font-family:var(--font-mono,ui-monospace,monospace); }
.resource-workspace--comfortable :deep(.resource-association-count) { min-width:27px; padding:2px 7px !important; border-radius:5px; background:var(--badge-blue-soft,var(--primary-soft)); color:var(--badge-blue,var(--primary)); font-size:12px; font-weight:600; }
.resource-workspace--comfortable :deep(.resource-association-count.is-purple) { color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); }
.resource-workspace--comfortable :deep(.resource-state) { font-size:12px; }
.resource-workspace--comfortable :deep(.resource-switch-cell) { flex-direction:row; justify-content:center; gap:5px; flex-wrap:wrap; }
.resource-workspace--comfortable :deep(.resource-switch-cell .resource-state) { font-size:12px; }
.resource-workspace--comfortable :deep(.resource-protocol) { color:var(--badge-amber,var(--warning)); background:var(--badge-amber-soft,var(--primary-soft)); font-size:12px; padding:3px 6px; }
.resource-workspace--comfortable :deep(.resource-table-footer) { padding:10px 15px; gap:8px; }
.resource-workspace--comfortable :deep(.resource-page-size), .resource-workspace--comfortable :deep(.resource-panel-note) { font-size:12px; }
.resource-workspace--comfortable :deep(.resource-panel-note) { padding:0 15px 11px; }
.resource-workspace--comfortable :deep(.resource-selection) { padding:8px 15px; font-size:12px; }
.resource-workspace--comfortable :deep(.tag-list) { gap:4px; }
.resource-workspace--comfortable :deep(.tag-list .el-tag) { height:24px; font-size:12px; padding:1px 6px; border-color:transparent; color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); }
.resource-workspace--comfortable :deep(.resource-mobile-card) { padding:12px; }
.resource-workspace--comfortable :deep(.resource-mobile-card-header > .resource-name) { font-size:12px; }
.resource-workspace--comfortable :deep(.resource-mobile-card-body) { margin-top:8px; font-size:12px; }
.resource-workspace--comfortable :deep(.resource-mobile-card-actions) { margin-top:8px; padding-top:8px; }
@media(max-width:767px) {
 .resource-workspace--comfortable :deep(.resource-panel-heading) { padding:12px 12px 0; }
 .resource-workspace--comfortable :deep(.resource-panel .table-toolbar) { padding:10px 12px; }
 .resource-workspace--comfortable :deep(.resource-table-footer) { padding:10px 12px; }
}
</style>
