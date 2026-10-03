// Explain observed failures without changing their recorded status or HTTP code.
export function getLogFailure(row = {}) {
  if (row.status === 'ok') return null
  // Use the server's final failure classification when it is available.
  // Other categories and legacy records retain their existing explanations.
  if (row.status === 'error') {
    switch (row.error_category) {
      case 'rate_limit': return { label: '限流或额度不足', explanation: '账号或服务触发频率、并发或额度限制，请结合原始错误等待重试或检查额度。' }
      case 'authentication': return { label: '认证或权限失败', explanation: '请求未通过认证或权限检查，请结合原始错误检查密钥、账号授权与访问范围。' }
      case 'overloaded': return { label: '上游繁忙', explanation: '上游服务器当前过载，未能完成请求。请稍后重试；HTTP 200 不代表模型已成功响应。' }
    }
  }
  const message = typeof row.error === 'string' ? row.error : ''
  const code = Number(row.http_code)
  if (/unsupported parameter/i.test(message)) return { label: '参数不兼容', explanation: '客户端发送了上游不接受的参数。检查客户端配置与网关兼容版本。' }
  if (/system messages are not allowed/i.test(message)) return { label: '消息角色不兼容', explanation: '此接口不接受 system 消息，需要使用兼容的消息角色。' }
  if (/no enabled account in the key.?s groups/i.test(message)) return { label: '无可用账号', explanation: '请求发生时，密钥可访问的分组中没有已启用账号。检查账号启用状态与分组关联。' }
  if (/invalidated oauth token|token.*revoked|refresh_token_reused/i.test(message)) return { label: '登录已失效', explanation: '上游拒绝了账号凭据。需要重新授权或更换有效凭据；此记录仍保留为失败。' }
  if (/model.*not supported|model_not_found/i.test(message)) return { label: '模型不可用', explanation: '该账号或接口不支持请求的模型。请使用此账号实际可用的模型名称。' }
  if (/encrypted content.*(?:verified|decrypted|parsed)|invalid_encrypted_content/i.test(message)) return { label: '会话内容失效', explanation: '上游无法验证加密历史。请从原账号与原会话继续，或创建新会话。' }
  if (code === 413 || /(?:request|body).*exceeds.*MiB|payload too large/i.test(message)) return { label: '请求过大', explanation: '请求超过大小限制。请缩小附件或输入内容后重新提交。' }
  if (/flagged for possible|content_filter|safety|policy violation/i.test(message)) return { label: '上游拒绝', explanation: '上游因内容策略拒绝了请求。HTTP 连接成功不代表模型请求成功，请查看原始错误。' }
  if (code === 429 || /rate.?limit|quota.*exceed/i.test(message)) return { label: '限流或额度不足', explanation: '账号或服务触发频率、并发或额度限制，请结合原始错误等待重试或检查额度。' }
  if (/servers are currently overloaded|server.?overloaded|overloaded_error/i.test(message)) return { label: '上游繁忙', explanation: '上游服务器当前过载，未能完成请求。请稍后重试；HTTP 200 不代表模型已成功响应。' }
  if (/an error occurred while processing your request/i.test(message)) return { label: '上游处理失败', explanation: '上游处理请求时发生错误。请查看原始错误和请求 ID，以便重试或向服务方排查。' }
  if (row.status === 'aborted' || code === 499 || /context canceled|client disconnect/i.test(message)) return { label: '请求中断', explanation: '请求在完成前被取消或连接中断，无法视为成功。' }
  if (code === 504 || /timeout|deadline exceeded|timed out/i.test(message)) return { label: '请求超时', explanation: '连接或响应超过等待时间。检查上游、代理与网络状态。' }
  if (code === 401 || code === 403) return { label: '认证或权限失败', explanation: '请求未通过认证或权限检查，请结合原始错误检查密钥、账号授权与访问范围。' }
  if (code >= 500) return { label: '服务异常', explanation: '服务未能完成请求，请查看原始错误定位上游、网络或网关问题。' }
  return { label: row.status === 'error' ? '请求失败' : '结果未知', explanation: '请查看完整错误与请求信息；缺少状态或用量不能推断请求成功。' }
}
