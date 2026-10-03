import assert from 'node:assert/strict'
import test from 'node:test'
import { getLogFailure } from '../src/utils/logFailure.js'

test('failure explanations cover observed errors without trusting transport status', () => {
  const samples = [
    [400, 'Unsupported parameter: max_output_tokens', '参数不兼容'],
    [400, 'Unsupported parameter: temperature', '参数不兼容'],
    [503, "no enabled account in the key's groups", '无可用账号'],
    [400, "The 'example' model is not supported when using Codex with a ChatGPT account.", '模型不可用'],
    [401, 'Encountered invalidated oauth token for user, failing request', '登录已失效'],
    [400, 'System messages are not allowed', '消息角色不兼容'],
    [400, 'The encrypted content could not be verified', '会话内容失效'],
    [413, 'request exceeds 32 MiB', '请求过大'],
    [200, 'This content was flagged for possible cybersecurity risk.', '上游拒绝'],
    [429, 'Rate limit exceeded', '限流或额度不足'],
    [200, 'Our servers are currently overloaded. Please try again later.', '上游繁忙'],
    [200, 'An error occurred while processing your request.', '上游处理失败'],
    [502, '<html><title>502 Bad Gateway</title></html>', '服务异常'],
  ]
  for (const [http_code, error, label] of samples) {
    const row = { status: 'error', http_code, error }
    const original = { ...row }
    assert.equal(getLogFailure(row).label, label)
    assert.deepEqual(row, original)
  }
  assert.equal(getLogFailure({ status: 'ok', http_code: 200 }), null)
  assert.equal(getLogFailure({ status: 'aborted', http_code: 200 }).label, '请求中断')
  assert.equal(getLogFailure({ status: 'error', error: '<img src=x onerror=alert(1)>' }).label, '请求失败')
  assert.equal(getLogFailure({}).label, '结果未知')
})

test('server categories explain actual final-generation failures consistently', () => {
  // These observations came from the local gateway producer, including SSE HTTP 200 failures.
  const samples = [
  {
    "name": "http_quota_auth_words",
    "record": {
      "status": "error",
      "http_code": 429,
      "error": "invalidated oauth in synthetic upstream message",
      "final_upstream_error_code": "rateLimitExceeded",
      "final_upstream_error_source": "upstream",
      "error_category": "rate_limit"
    },
    "expected": "限流或额度不足"
  },
  {
    "name": "sse_busy_code_auth_type",
    "record": {
      "status": "error",
      "http_code": 200,
      "error": "bps response terminated with response.failed (status failed) (code=server_overloaded) (error_type=authentication_error)",
      "final_upstream_error_code": "unauthorized",
      "final_upstream_error_source": "upstream",
      "error_category": "authentication"
    },
    "expected": "认证或权限失败"
  },
  {
    "name": "sse_model_code_rate_type",
    "record": {
      "status": "error",
      "http_code": 200,
      "error": "bps response terminated with response.failed (status failed) (code=model_not_found) (error_type=rate_limit_error)",
      "final_upstream_error_code": "rateLimitExceeded",
      "final_upstream_error_source": "upstream",
      "error_category": "rate_limit"
    },
    "expected": "限流或额度不足"
  },
  {
    "name": "sse_quota_control",
    "record": {
      "status": "error",
      "http_code": 200,
      "error": "bps response terminated with response.failed (status failed) (code=rate_limit_exceeded)",
      "final_upstream_error_code": "rateLimitExceeded",
      "final_upstream_error_source": "upstream",
      "error_category": "rate_limit"
    },
    "expected": "限流或额度不足"
  },
  {
    "name": "sse_type_only_auth",
    "record": {
      "status": "error",
      "http_code": 200,
      "error": "bps response terminated with response.failed (status failed) (error_type=authentication_error)",
      "final_upstream_error_code": "unauthorized",
      "final_upstream_error_source": "upstream",
      "error_category": "authentication"
    },
    "expected": "认证或权限失败"
  }
]
  for (const { name, record, expected } of samples) {
    const before = structuredClone(record)
    assert.equal(getLogFailure(record).label, expected, name)
    assert.deepEqual(record, before, name)
  }
})

test('management explanations preserve successful, aborted and legacy observations', () => {
  assert.equal(getLogFailure({ status: 'ok', error_category: 'rate_limit', error: 'model_not_found', http_code: 200 }), null)
  assert.equal(getLogFailure({ status: 'aborted', error_category: 'rate_limit', error: 'context canceled', http_code: 499 }).label, '请求中断')
  assert.equal(getLogFailure({ status: 'error', error: 'model_not_found', http_code: 200 }).label, '模型不可用')
  assert.equal(getLogFailure({ status: 'error', error_category: 'future_category', error: 'model_not_found', http_code: 200 }).label, '模型不可用')
  assert.equal(getLogFailure({ status: 'error', error_category: 'overloaded', error: 'rate_limit_error', http_code: 200 }).label, '上游繁忙')
})
