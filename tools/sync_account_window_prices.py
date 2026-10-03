"""Freeze Sub2API's OpenAI catalog and explicit Go fallbacks for account estimates.

Run: python tools/sync_account_window_prices.py --source ../sub2api/current-source
No network, guessed promotions, or billing configuration is involved.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
from datetime import date


def capture(source):
    catalog = source / 'backend/resources/model-pricing/model_prices_and_context_window.json'
    billing = source / 'backend/internal/service/billing_service.go'
    raw = json.loads(catalog.read_text(encoding='utf-8'))
    models = {name: {key: value for key, value in entry.items()
                     if isinstance(value, (int, float)) and not isinstance(value, bool)
                     and ('cost' in key or key == 'long_context_input_token_threshold')}
              for name, entry in raw.items() if name.startswith(('gpt-', 'o1', 'o3', 'o4'))
              and 'input_cost_per_token' in entry and 'output_cost_per_token' in entry}
    fields = {'InputPricePerToken': 'input_cost_per_token', 'OutputPricePerToken': 'output_cost_per_token',
              'CacheReadPricePerToken': 'cache_read_input_token_cost', 'CacheCreationPricePerToken': 'cache_creation_input_token_cost',
              'InputPricePerTokenPriority': 'input_cost_per_token_priority', 'OutputPricePerTokenPriority': 'output_cost_per_token_priority',
              'CacheReadPricePerTokenPriority': 'cache_read_input_token_cost_priority', 'CacheCreationPricePerTokenPriority': 'cache_creation_input_token_cost_priority',
              'LongContextInputThreshold': 'long_context_input_token_threshold', 'LongContextInputMultiplier': 'long_context_input_cost_multiplier',
              'LongContextOutputMultiplier': 'long_context_output_cost_multiplier'}
    for name, body in re.findall(r's\.fallbackPrices\["([^"]+)"\] = &ModelPricing\{(.*?)\n\t\}', billing.read_text(encoding='utf-8'), re.S):
        if name in models or not name.startswith(('gpt-', 'o1', 'o3', 'o4')):
            continue
        card = {}
        for field, value in re.findall(r'^\s*(\w+):\s*([0-9eE_.+-]+),', body, re.M):
            if field in fields:
                card[fields[field]] = float(value.replace('_', ''))
        if 'input_cost_per_token' in card and 'output_cost_per_token' in card:
            models[name] = card
    return {'source': 'sub2api/current-source: catalog then billing_service.go fallback',
            'captured_at': date.today().isoformat(), 'source_sha256': hashlib.sha256(catalog.read_bytes()).hexdigest(),
            'billing_source_sha256': hashlib.sha256(billing.read_bytes()).hexdigest(), 'models': dict(sorted(models.items()))}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', type=Path, default=Path('../sub2api/current-source'))
    parser.add_argument('--output', type=Path, default=Path(__file__).resolve().parents[1] / 'internal/server/account_window_prices.json')
    args = parser.parse_args()
    result = capture(args.source)
    args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'models': len(result['models']), 'source_sha256': result['source_sha256'], 'output': str(args.output)}))
