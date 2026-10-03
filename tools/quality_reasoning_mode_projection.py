"""One explicit documented reasoning.mode field; no model/credential operations."""
from copy import deepcopy
import hashlib
import json
from quality_cache_key_projection import _object, _validate_head, MAX_BODY

def sha(value):return hashlib.sha256(value).hexdigest()

def project_mode(body, head, mode):
    if mode not in ('standard','pro'):raise ValueError('mode_not_registered')
    value, spans = _object(body);_validate_head(body,head)
    if (value.get('model')!='gpt-5.6-sol' or value.get('stream') is not True or value.get('store') is not False
            or 'previous_response_id' in value or not isinstance(value.get('input'),list)
            or not isinstance(value.get('reasoning'),dict) or value['reasoning'].get('effort')!='medium'):
        raise ValueError('unexpected_original_request')
    start,end=spans['reasoning'];reasoning,inner=_object(body[start:end])
    if 'mode' in reasoning:
        if reasoning['mode']!='standard':raise ValueError('original_mode_not_standard')
        left,right=inner['mode'];left+=start;right+=start
        insertion=body[left:right] if mode=='standard' else json.dumps(mode).encode()
        action='replace_existing_mode'
    else:
        left=right=end-1
        if body[left:right+1]!=b'}':raise ValueError('reasoning_span_invalid')
        insertion=(b',' if reasoning else b'')+b'"mode":'+json.dumps(mode).encode()
        action='insert_explicit_mode'
    changed=body[:left]+insertion+body[right:]
    if len(changed)>MAX_BODY:raise ValueError('body_limit')
    updated=deepcopy(head);updated.update(body_sha256=sha(changed),body_bytes=len(changed))
    result,_=_object(changed);_validate_head(changed,updated)
    assert result['reasoning'].pop('mode')==mode
    original=deepcopy(value);original['reasoning'].pop('mode',None)
    assert result==original
    return changed,updated,{'schema':'reasoning-mode-projection-v1','action':action,'mode':mode,
        'original_body_sha256':sha(body),'projected_body_sha256':sha(changed),'body_bytes':len(changed),
        'span':{'start':left,'end':right,'replacement_bytes':len(insertion)},
        'unchanged_prefix_sha256':sha(body[:left]),'unchanged_suffix_sha256':sha(body[right:]),
        'other_bytes_unchanged':True,'non_auth_headers_unchanged':True,'endpoint_acceptance_not_assumed':True,
        'model_calls':0,'network_calls':0}
