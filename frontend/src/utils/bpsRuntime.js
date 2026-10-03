export const productName = '高级反代系统'

// Display only values returned by the running service. Defaults in deployment
// documentation must not be mistaken for this instance's effective settings.
export function inferenceRuntime(value) {
  const source = value && typeof value === 'object' ? value : {}
  const labels = { powerpoint_bps: 'PowerPoint + Codex', official_codex_cli: 'Codex 直连' }
  const known = Object.hasOwn(labels, source.backend)
  let baseURL = ''
  if (typeof source.base_url === 'string') {
    try {
      const url = new URL(source.base_url)
      if (['http:', 'https:'].includes(url.protocol) && !url.username && !url.password) baseURL = `${url.origin}${url.pathname}`
    } catch { /* Invalid runtime data is displayed as unavailable. */ }
  }
  return {
    known,
    label: known ? labels[source.backend] : '服务配置待读取',
    mode: known ? source.backend : '',
    baseURL,
    toolsVersion: typeof source.tools_version === 'string' ? source.tools_version : '',
    search: source.web_search === 'enabled' ? '已开启' : source.web_search === 'disabled' ? '已关闭' : '未提供',
    codexFallback: source.codex_fallback === false ? '未启用' : source.codex_fallback === true ? '已启用' : '未提供',
  }
}
