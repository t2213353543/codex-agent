import test from 'node:test'
import assert from 'node:assert/strict'
import { logSeverity, managementSeveritySource, severityDistribution } from '../src/utils/logSeverity.js'
import { managedQuerySnapshot, readManagedFilters, managementScopeParams } from '../src/utils/logManagementState.js'
import { errorReviewQuery } from '../src/utils/logErrorReviewState.js'
import { cleanupSnapshot } from '../src/utils/logCleanupState.js'
import { sanitizedLogRecord } from '../src/utils/logExport.js'

test('severity consumes authoritative management metadata without replacing HTTP or inferring type exceptions', () => {
  for (const [http, category, severity] of [[200, 'overloaded', 'P1'], [500, 'server_error', 'P1'], [500, 'unsupported_model', 'P3'], [413, 'request_too_large', 'P2'], [0, 'unknown', 'unknown']]) {
    const record = { status: 'error', http_code: http, error_category: category, error_severity: severity, error_severity_source: managementSeveritySource }
    assert.equal(logSeverity(record).value, severity)
    assert.equal(record.http_code, http)
    assert.equal(record.status, 'error')
    assert.match(logSeverity(record).explanation, /管理规则 v1/)
    assert.match(logSeverity(record).explanation, /上游原生分级/)
  }
  assert.equal(logSeverity({ status: 'ok', error_severity: 'P1', error_severity_source: managementSeveritySource }), null)
  assert.equal(logSeverity({ status: 'error', http_code: 500 }).value, 'unknown')
})

test('unverified severity sources or values never become trusted labels or exported source text', () => {
  for (const source of [undefined, null, 'response', '<img src=x onerror=alert(1)>']) {
    const row = { status: 'error', error_severity: 'P1', error_severity_source: source }
    assert.equal(logSeverity(row).value, 'unknown')
    assert.match(logSeverity(row).explanation, /来源未验证/)
    assert.equal(sanitizedLogRecord(row).error_severity_source, null)
    assert.equal(sanitizedLogRecord(row).error_severity, 'unknown')
  }
  assert.equal(logSeverity({ status: 'aborted', error_severity: 'p1', error_severity_source: managementSeveritySource }).value, 'unknown')
  assert.equal('error_severity' in sanitizedLogRecord({ status: 'ok' }), false)
})

test('all four severity filters survive URL, snapshots, cleanup and independent review overrides', () => {
  for (const severity of ['P1', 'P2', 'P3', 'unknown']) {
    const snapshot = managedQuerySnapshot({ error_severity: severity, from: '100', to: '200', account_id: 'stable-account' })
    assert.equal(snapshot.params.error_severity, severity)
    assert.equal(readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params))).error_severity, severity)
    assert.equal(managementScopeParams(snapshot.params).error_severity, severity)
    assert.equal(cleanupSnapshot(snapshot.params, '100', '200').error_severity, severity)
    assert.equal(errorReviewQuery(snapshot.params).error_severity, severity)
    assert.equal(errorReviewQuery(snapshot.params, 'false', '', 20, '').error_severity, undefined)
    assert.equal(errorReviewQuery(snapshot.params, 'false', '', 20, 'P2').error_severity, 'P2')
  }
  assert.equal(readManagedFilters({ error_severity: 'p1' }).error_severity, '')
  assert.equal(readManagedFilters({ error_severity: 'critical' }).error_severity, '')
})

test('severity distribution requires the declared management source and validated categories', () => {
  const rows = [{ name: 'P1', total_requests: 2 }, { name: 'unknown', total_requests: 1 }]
  assert.deepEqual(severityDistribution({ error_severity_source: managementSeveritySource, error_severities: rows }).rows, rows)
  assert.deepEqual(severityDistribution({ error_severity_source: 'upstream', error_severities: rows }).rows, [])
  assert.match(severityDistribution({ error_severity_source: 'upstream', error_severities: rows }).note, /来源未验证/)
  assert.deepEqual(severityDistribution({ error_severity_source: managementSeveritySource, error_severities: [{ name: '<script>' }] }).rows, [])
  assert.deepEqual(severityDistribution({ error_severity_source: managementSeveritySource, error_severities: [] }).rows, [])
})
