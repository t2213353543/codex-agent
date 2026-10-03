"""Offline exact reasoning.effort intervention; no networking or credential I/O."""
from copy import deepcopy
import hashlib
import json

from quality_cache_key_projection import _object, _validate_head, CacheKeyProjectionError, MAX_BODY

EFFORTS=frozenset(('none','minimal','low','medium','high','xhigh','max','ultra','persistent'))

class EffortProjectionError(ValueError):
    pass

def sha(raw):return hashlib.sha256(raw).hexdigest()

def project_effort(body,head,new_effort,*,expected_original='medium',supported_efforts=()):
    """Change only the existing top-level reasoning object's effort value span.

    supported_efforts must come from the caller's registered model-catalog
    observation. This validates a declared experiment constraint, not server
    acceptance, compute allocation, or effectiveness of an effort value.
    """
    if type(new_effort) is not str or new_effort not in EFFORTS or expected_original not in EFFORTS:
        raise EffortProjectionError('unsupported_effort_label')
    if not isinstance(supported_efforts,(list,tuple,set,frozenset)) or any(type(v) is not str or v not in EFFORTS for v in supported_efforts):
        raise EffortProjectionError('invalid_declared_supported_efforts')
    if new_effort not in supported_efforts or expected_original not in supported_efforts:
        raise EffortProjectionError('effort_not_in_declared_catalog')
    try:
        value,spans=_object(body);_validate_head(body,head)
        if (value.get('stream') is not True or value.get('store') is not False
                or 'previous_response_id' in value or type(value.get('input')) is not list
                or type(value.get('model')) is not str or not value['model']
                or type(value.get('reasoning')) is not dict):
            raise EffortProjectionError('requires_full_first_turn_http')
        a,b=spans['reasoning'];reasoning,inner=_object(body[a:b])
        if reasoning.get('effort')!=expected_original or type(reasoning.get('effort')) is not str:
            raise EffortProjectionError('original_effort_mismatch')
        first,last=inner['effort'];first+=a;last+=a
        # Equal control keeps the exact original spelling, including escapes.
        encoded=body[first:last] if new_effort==expected_original else json.dumps(new_effort).encode()
        changed=body[:first]+encoded+body[last:]
        if len(changed)>MAX_BODY:raise EffortProjectionError('projected_body_limit')
        updated=deepcopy(head);updated.update(body_sha256=sha(changed),body_bytes=len(changed))
        after,after_spans=_object(changed);_validate_head(changed,updated)
        assert after['reasoning']['effort']==new_effort
        # The invariant is lexical, so unknown fields, exact numbers and all
        # metadata survive without JSON tree reserialization.
        proof={'schema':'codex-quality-effort-projection-v1','model_calls':0,'network_calls':0,
            'scope':'only_existing_reasoning_effort_value_bytes','original_effort':expected_original,'requested_effort':new_effort,
            'declared_supported_efforts':sorted(set(supported_efforts)),'original_body_sha256':sha(body),
            'projected_body_sha256':sha(changed),'body_bytes':len(changed),'body_changed':changed!=body,
            'value_span':{'original_start':first,'original_end':last,'projected_start':first,'projected_end':first+len(encoded)},
            'original_value_sha256':sha(body[first:last]),'projected_value_sha256':sha(encoded),
            'prefix_sha256':sha(body[:first]),'suffix_sha256':sha(body[last:]),
            'non_effort_bytes_unchanged':True,'headers_unchanged':True,'server_acceptance_observed':False}
        return changed,updated,proof
    except EffortProjectionError:
        raise
    except (CacheKeyProjectionError,KeyError,AssertionError,TypeError,UnicodeError) as error:
        raise EffortProjectionError('invalid_capture_or_effort_structure') from error

__all__=['project_effort','EffortProjectionError']
