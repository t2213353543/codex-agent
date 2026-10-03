"""Execute Sub2API's original token-cost functions, then compare this gateway.

Run: python tools/verify_account_window_parity.py --source ../sub2api/current-source
No account credentials, production database, or network are used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

from sync_account_window_prices import capture

ROOT = Path(__file__).resolve().parents[1]


def declaration(text, kind, name):
    pattern = rf'(?ms)^{kind} (?:\([^\n]+\) )?{re.escape(name)}\b.*?^}}'
    match = re.search(pattern, text)
    if not match:
        raise ValueError(f'Sub2API declaration not found: {kind} {name}')
    return match.group()


def verify(source, output):
    output.mkdir(parents=True, exist_ok=True)
    billing_path = source / 'backend/internal/service/billing_service.go'
    billing = billing_path.read_text(encoding='utf-8')
    prices = capture(source)
    embedded = json.loads((ROOT / 'internal/server/account_window_prices.json').read_text(encoding='utf-8'))
    if prices['models'] != embedded['models']:
        raise ValueError('Embedded price catalog differs from current Sub2API extraction; sync prices first')
    types = ['ModelPricing', 'UsageTokens', 'CostBreakdown']
    functions = ['normalizeBillingServiceTier', 'usePriorityServiceTierPricing',
                 'serviceTierCostMultiplier', 'configuredServiceTierMultiplier',
                 'computeTokenBreakdown', 'computeCacheCreationCost', 'normalizeCacheCreationBreakdown',
                 'longContextMultiplierOrOne', 'shouldApplySessionLongContextPricing',
                 'openAIModelFastPricingRatio', 'enforceOpenAIFastPricingRatio']
    originals = [declaration(billing, 'type', name) for name in types]
    originals += [declaration(billing, 'func', name) for name in functions]
    # Only canonical OpenAI names are tested. Non-OpenAI and alias resolution are
    # outside this harness; original cost functions and Fast policy are unchanged.
    program = '''package main
import ("encoding/json"; "os"; "math"; "strings")
type BillingService struct{}
const OpenAIFastTierUltrafast = "ultrafast"
func isOpenAIGPT6AstraModel(m string) bool { return m == "gpt-6-astra" }
''' + '\n\n'.join(originals) + '''
type Case struct {
 Model string `json:"model"`; Tier string `json:"tier"`
 Input int64 `json:"input"`; Cached int64 `json:"cached"`; Output int64 `json:"output"`
 Write int64 `json:"write"`; ImageInput int64 `json:"image_input"`; ImageOutput int64 `json:"image_output"`
 Pricing ModelPricing `json:"pricing"`; Expected float64 `json:"expected"`
}
func main() {
 var cases []Case
 if err := json.NewDecoder(os.Stdin).Decode(&cases); err != nil { panic(err) }
 for i := range cases {
  c := &cases[i]; p := &c.Pricing
  // Canonical GPT-5.6 cache-creation policy from applyModelSpecificPricingPolicyEx.
  if strings.HasPrefix(c.Model, "gpt-5.6-") && p.CacheCreationPricePerToken <= 0 {
   p.CacheCreationPricePerToken = p.InputPricePerToken * 1.25
  }
  enforceOpenAIFastPricingRatio(p, openAIModelFastPricingRatio(c.Model))
  tokens := UsageTokens{InputTokens: int(c.Input-c.Cached-c.Write), CacheReadTokens: int(c.Cached),
   CacheCreationTokens: int(c.Write), OutputTokens: int(c.Output), ImageInputTokens: int(c.ImageInput), ImageOutputTokens: int(c.ImageOutput)}
  c.Expected = (&BillingService{}).computeTokenBreakdown(p, tokens, 1, c.Tier, true).TotalCost
 }
 if err := json.NewEncoder(os.Stdout).Encode(cases); err != nil { panic(err) }
}
'''
    fields = {'input_cost_per_token': 'InputPricePerToken', 'output_cost_per_token': 'OutputPricePerToken',
              'cache_read_input_token_cost': 'CacheReadPricePerToken', 'cache_creation_input_token_cost': 'CacheCreationPricePerToken',
              'input_cost_per_token_priority': 'InputPricePerTokenPriority', 'output_cost_per_token_priority': 'OutputPricePerTokenPriority',
              'cache_read_input_token_cost_priority': 'CacheReadPricePerTokenPriority', 'cache_creation_input_token_cost_priority': 'CacheCreationPricePerTokenPriority',
              'input_cost_per_image_token': 'ImageInputPricePerToken', 'output_cost_per_image_token': 'ImageOutputPricePerToken',
              'long_context_input_token_threshold': 'LongContextInputThreshold', 'long_context_input_cost_multiplier': 'LongContextInputMultiplier',
              'long_context_output_cost_multiplier': 'LongContextOutputMultiplier'}
    models = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna', 'gpt-5.5', 'gpt-5.5-pro',
              'gpt-5.4', 'gpt-5.4-mini', 'gpt-5.4-nano', 'gpt-4o', 'o3',
              'gpt-image-1.5', 'gpt-image-2', 'gpt-image-2.5-flare']
    cases = []
    for model in models:
        card = prices['models'][model]
        pricing = {fields[key]: value for key, value in card.items() if key in fields}
        if 'LongContextInputThreshold' in pricing:
            pricing['LongContextInputThreshold'] = int(pricing['LongContextInputThreshold'])
        # PricingService derives this catalog's absolute 272k tier to multipliers.
        if not any(key.startswith('long_context_') for key in card) and 'input_cost_per_token_above_272k_tokens' in card:
            pricing.update(LongContextInputThreshold=272000,
                           LongContextInputMultiplier=card['input_cost_per_token_above_272k_tokens']/card['input_cost_per_token'],
                           LongContextOutputMultiplier=card.get('output_cost_per_token_above_272k_tokens', card['output_cost_per_token'])/card['output_cost_per_token'])
        for tier in ['', 'default', 'fast', 'priority', 'flex', 'ultrafast']:
            for inp, cached, out, write, image_in, image_out in [(0,0,0,0,0,0), (100000,80000,1000,0,0,0),
                    (272000,200000,1000,0,0,0), (272001,200000,1000,0,0,0), (300000,200000,1000,10000,1000,100),
                    (100000,30000,1000,20000,10000,100), (1,0,1,0,0,0)]:
                cases.append(dict(model=model,tier=tier,input=inp,cached=cached,output=out,write=write,image_input=image_in,image_output=image_out,pricing=pricing))
    program_path = output / 'sub2api_original_cost.go'
    program_path.write_text(program, encoding='utf-8')
    reference = subprocess.run(['go', 'run', str(program_path)], input=json.dumps(cases), text=True, encoding='utf-8', capture_output=True)
    if reference.returncode:
        raise RuntimeError(reference.stderr)
    reference_path = output / 'sub2api-cost-reference.json'
    reference_path.write_text(reference.stdout, encoding='utf-8')
    env = os.environ.copy()
    env['CODEX_SUB2API_COST_REFERENCE'] = str(reference_path.resolve())
    check = subprocess.run(['go', 'test', './internal/server', '-run', '^TestManagedAccountSub2APIExecutableParity$', '-count=1', '-json'],
                           cwd=ROOT, env=env, capture_output=True, text=True, encoding='utf-8')
    (output / 'sub2api-cost-parity-go.jsonl').write_text(check.stdout, encoding='utf-8')
    report = {'status': 'pass' if check.returncode == 0 else 'fail', 'cases': len(cases), 'models': models,
              'original_functions': functions, 'source_sha256': prices['source_sha256'], 'billing_source_sha256': prices['billing_source_sha256'],
              'program_sha256': hashlib.sha256(program_path.read_bytes()).hexdigest(), 'catalog_exact_match': True,
              'scope': 'Original Sub2API token-cost engine; canonical OpenAI models, account multiplier 1, default prices, long-context enabled',
              'go_exit_code': check.returncode}
    (output / 'sub2api-cost-parity.json').write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n', encoding='utf-8')
    print(json.dumps(report, ensure_ascii=False))
    if check.returncode:
        print(check.stdout)
        raise SystemExit(check.returncode)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', type=Path, default=ROOT.parent / 'sub2api/current-source')
    parser.add_argument('--output', type=Path, default=ROOT / '.local/reliability-modes')
    args = parser.parse_args()
    verify(args.source.resolve(), args.output.resolve())
