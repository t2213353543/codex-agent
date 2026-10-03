"""Offline answer-blind semantics projection: exactly one user text JSON token.

The body remains the original CLI full request, including all developer/system
items, tools, numeric spellings, metadata and non-auth headers. Baseline is an
exact no-op, including the original string token's escape spelling. Variant
answer keys and proof are evidence only and never inserted into model input.
"""
from __future__ import annotations
from copy import deepcopy
import hashlib,json
import quality_cache_key_projection as strict
from quality_semantics_variants import request_content,ORIGINAL_CAPABILITY,CLARIFIED_CAPABILITY,BLIND_CAPABILITY,VARIANT_IDS
from mother_account_benchmark import CANDY_PROMPT

SCHEMA='codex-quality-prompt-variant-projection-v1'
BASE_PROMPT_SHA='cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f'
class PromptVariantProjectionError(Exception):
    """Fixed error labels only; untrusted prompt/JSON values never appear."""
def sha(raw):return hashlib.sha256(raw).hexdigest()

def _string_spans(raw):
    # _object has already validated duplicate keys, Unicode, numeric lexical
    # bounds, nodes and depth. Traverse that validated spelling for byte spans.
    text=raw.decode('utf-8');matches=[]
    def whitespace(i):
        while i<len(text) and text[i] in ' \t\r\n':i+=1
        return i
    def walk(i,path):
        i=whitespace(i);start=i
        if text[i]=='{':
            i=whitespace(i+1)
            if text[i]=='}':return i+1
            while True:
                name,i=strict.DECODER.raw_decode(text,i);i=whitespace(i)
                if text[i]!=':':raise PromptVariantProjectionError('invalid_json')
                i=whitespace(walk(i+1,path+(name,)))
                if text[i]=='}':return i+1
                i=whitespace(i+1)
        elif text[i]=='[':
            i=whitespace(i+1);index=0
            if text[i]==']':return i+1
            while True:
                i=whitespace(walk(i,path+(index,)));index+=1
                if text[i]==']':return i+1
                i=whitespace(i+1)
        else:
            value,i=strict.DECODER.raw_decode(text,i)
            if type(value) is str and value==CANDY_PROMPT:
                if matches:raise PromptVariantProjectionError('original_prompt_not_unique')
                matches.append((path,len(text[:start].encode()),len(text[:i].encode())))
            return i
    walk(0,())
    return matches

def _validate(body,captured):
    try:
        value,spans=strict._object(body)
        strict._validate_head(body,captured)
        if value.get('stream') is not True or value.get('store') is not False or 'previous_response_id'in value or type(value.get('model')) is not str or not value['model'].strip() or type(value.get('input')) is not list:raise PromptVariantProjectionError('requires_full_first_turn')
        matches=_string_spans(body)
        if len(matches)!=1:raise PromptVariantProjectionError('original_prompt_not_unique')
        path,start,end=matches[0]
        if len(path)!=5 or path[0]!='input' or type(path[1]) is not int or path[2]!='content' or type(path[3]) is not int or path[4]!='text':raise PromptVariantProjectionError('original_prompt_not_user_input_text')
        item=value['input'][path[1]];part=item['content'][path[3]]
        if item.get('role')!='user' or item.get('type','message')!='message' or part.get('type')!='input_text':raise PromptVariantProjectionError('original_prompt_not_user_input_text')
        return value,spans,path,start,end
    except PromptVariantProjectionError:raise
    except strict.CacheKeyProjectionError as error:raise PromptVariantProjectionError(error.args[0]) from None
    except (TypeError,ValueError,IndexError,KeyError,UnicodeError,RecursionError):raise PromptVariantProjectionError('invalid_prompt_capture') from None

def project_prompt_variant(body,captured_head,variant_id):
    """Return (body_bytes, detached_head, safe_evidence); performs no network I/O."""
    if type(variant_id) is not str or variant_id not in VARIANT_IDS:raise PromptVariantProjectionError('unknown_variant')
    if sha(CANDY_PROMPT.encode())!=BASE_PROMPT_SHA:raise PromptVariantProjectionError('base_prompt_contract_changed')
    value,spans,path,start,end=_validate(body,captured_head)
    try:replacement=request_content(variant_id)['input']
    except (TypeError,ValueError):raise PromptVariantProjectionError('variant_contract_changed') from None
    if variant_id=='baseline':encoded=body[start:end]
    else:encoded=json.dumps(replacement,ensure_ascii=False,separators=(',',':')).encode()
    if len(body)-(end-start)+len(encoded)>strict.MAX_BODY:raise PromptVariantProjectionError('projected_body_limit')
    projected=body[:start]+encoded+body[end:];newend=start+len(encoded)
    changed=deepcopy(captured_head);changed.update(body_sha256=sha(projected),body_bytes=len(projected))
    try:
        after,afterspans=strict._object(projected);strict._validate_head(projected,changed)
    except strict.CacheKeyProjectionError as error:raise PromptVariantProjectionError(error.args[0]) from None
    aftertext=after['input'][path[1]]['content'][path[3]]['text']
    if aftertext!=replacement or projected[:start]!=body[:start] or projected[newend:]!=body[end:]:raise PromptVariantProjectionError('projection_crossed_string_boundary')
    # Exact canonical comparison is an extra guard. It preserves JSON numeric
    # lexemes rather than converting large integers/exponents through float.
    check=deepcopy(after);check['input'][path[1]]['content'][path[3]]['text']=CANDY_PROMPT
    if strict._canonical(check)!=strict._canonical(value):raise PromptVariantProjectionError('projection_changed_other_value')
    fields=[]
    for name in sorted(spans):
        before_raw=body[slice(*spans[name])];after_raw=projected[slice(*afterspans[name])]
        fields.append({'name':name if name in strict.KNOWN_FIELDS else 'name_sha256:'+sha(name.encode()),'name_sha256':sha(name.encode()),'original_value_sha256':sha(before_raw),'projected_value_sha256':sha(after_raw),'raw_value_unchanged':before_raw==after_raw})
    stable_head={k:v for k,v in captured_head.items() if k not in ('body_sha256','body_bytes')}
    evidence={'schema':SCHEMA,'variant_id':variant_id,'task_relation':'original' if variant_id=='baseline' else 'diagnostic_only','original_benchmark_eligible':variant_id=='baseline','scope':'one_exact_user_input_text_JSON_string_token','model_calls':0,'network_calls':0,'source_pointer':'/input/%d/content/%d/text'%(path[1],path[3]),'source_path':list(path),'original_prompt_sha256':BASE_PROMPT_SHA,'projected_prompt_sha256':sha(replacement.encode()),'original_body_sha256':sha(body),'projected_body_sha256':sha(projected),'original_body_bytes':len(body),'projected_body_bytes':len(projected),'body_bytes_changed':body!=projected,'text_span':{'original_start':start,'original_end':end,'projected_start':start,'projected_end':newend,'original_token_sha256':sha(body[start:end]),'projected_token_sha256':sha(encoded)},'prefix':{'sha256':sha(body[:start]),'bytes':start,'unchanged':True},'suffix':{'sha256':sha(body[end:]),'bytes':len(body)-end,'unchanged':True},'all_other_body_bytes_unchanged':True,'non_prompt_semantic_values_unchanged':True,'top_level_fields':fields,'head_change_fields':[] if variant_id=='baseline' else ['body_sha256','body_bytes'],'all_non_body_head_fields_unchanged':True,'unchanged_head_fields_canonical_sha256':sha(strict._canonical(stable_head)),'original_prompt_match_count':1,'answer_key_inserted_into_body':False,'instructions_fields_unchanged':True,'signature_or_provider_acceptance_verified':False}
    return projected,changed,evidence

__all__=['PromptVariantProjectionError','project_prompt_variant','BASE_PROMPT_SHA']
