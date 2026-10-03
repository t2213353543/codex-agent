"""Run management race tests on a frozen Go snapshot and synthetic PG/Redis.

Reuse the existing restricted integration runner and its pinned compiler image.
No production data, inference, service switch, package installation or gateway
load test is involved. This wrapper does not edit the gateway owner's runner.
"""
from __future__ import annotations

import hashlib
import re

import gateway_source_race as runner


REQUIRED = {
    "TestPGManagedCleanupLifecycle",
    "TestPGManagedUsageSnapshot",
    "TestPGManagedAutoReset",
    "TestPGManagedLeaseWriteFailureAtomic",
    "TestPGManagedAccountIdentityPrivacyBatch",
    "TestPGManagedCRSSync",
    "TestPGManagedAccountTestStream",
    "TestPGManagedUsageOptionsCalendar",
    "TestPGManagedEmptyUsageSnapshotStaysEmpty",
    "TestPGManagedUsageErrorReview",
    "TestPGManagedAccountRuntimeReadOnly",
    "TestPGManagedAccountLastUsedStableID",
    "TestManagedObservedUsageMetadataContract",
    "TestPGManagedBundleLeaseRestore",
    "TestPGManagedBundleLeaseValidationAndPartial",
    "TestPGManagedExternalLeaseImport",
    "TestPGManagedObservationQuery",
    "TestPGManagedObservationRuntimeToQuery",
    "TestPGManagedBillingDisabled",
    "TestPGManagedUsageWithoutBilling",
    "TestPGManagedUsageAggregationParityReview",
    "TestPGManagedUsageTimeOrder",
    "TestPGManagedUsageTimeIndexPlan",
    "TestPGManagedUsageOptionIdentityParity",
    "TestPGManagedAccountLastUsedSort",
    "TestPGManagedCRSAtomicCommit",
    "TestPGManagedScheduleIdempotencyAndOrphans",
    "TestPGManagedExpiredLeaseNoop",
    "TestPGManagedAccountIdentityStatusSort",
    "TestPGManagedAccountSummaryStableIdentity",
    "TestPGManagedAccountLargeMetadataExport",
    "TestPGManagedUsageWebSocketWarmup",
    "TestPGManagedCleanupObservationFilters",
    "TestPGManagedSeverityContractMatrix",
    "TestPGManagedUsageSeverity",
    "TestPGManagedPassiveClientIDQuery",
    "TestGatewayPassiveIngressDoesNotChangeWireOrUsage",
    "TestGatewayPassiveImageHTTPWire",
    "TestGatewayPassiveImageWebSocketIsolation",
    "TestPGManagedIngressEvents",
    "TestPGManagedPassiveRuntimeToQuery",
    "TestPGManagedPassiveUsageDetails",
}


def management_tests(manifest: dict, source_root=None) -> list[str]:
    source_root = source_root or runner.ROOT
    selected: set[str] = set()
    for relative, expected in manifest["files"].items():
        if not relative.rsplit("/", 1)[-1].startswith(("admin_", "gateway_passive_", "gateway_ingress_")) or not relative.endswith("_test.go"):
            continue
        raw = (source_root / relative).read_bytes()
        if hashlib.sha256(raw).hexdigest() != expected:
            raise RuntimeError("Management source changed while selecting tests")
        for name in re.findall(r"^func (Test\w+)\(t \*testing.T\)", raw.decode("utf-8"), re.M):
            if name.startswith(("TestManaged", "TestPGManaged", "TestManagementState", "TestPGManagementState", "TestGatewayPassive", "TestGatewayIngress")):
                selected.add(name)
    if not REQUIRED <= selected:
        raise RuntimeError("Management race selector omitted required integration coverage")
    return sorted(selected)


if __name__ == "__main__":
    runner.OUTPUT_ROOT = runner.ROOT / ".local/management-parity"
    runner.selected_tests = management_tests
    runner.REQUIRED_TESTS = REQUIRED
    raise SystemExit(runner.main())
