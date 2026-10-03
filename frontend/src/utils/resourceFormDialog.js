import { onBeforeUnmount } from 'vue'
import { ElMessageBox } from 'element-plus'

// Only the initial draft is retained in memory; it is cleared with the view.
export function useResourceFormDialog({ readDraft, isBusy, hide }) {
  let initial = ''
  let confirming = false
  function capture() { initial = JSON.stringify(readDraft()) }
  async function close(done) {
    if (isBusy() || confirming) return false
    if (initial !== JSON.stringify(readDraft())) {
      confirming = true
      try {
        await ElMessageBox.confirm('当前修改尚未保存，关闭后将丢弃这些修改。', '放弃修改？', {
          type: 'warning', confirmButtonText: '放弃修改', cancelButtonText: '继续编辑', closeOnClickModal: false,
        })
      } catch { return false }
      finally { confirming = false }
    }
    if (typeof done === 'function') done()
    else hide()
    return true
  }
  onBeforeUnmount(() => { initial = '' })
  return { capture, close }
}
