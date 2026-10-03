import json
import unittest
from bps_live_recovery_evidence import verify_logs


class RecoveryEvidenceTests(unittest.TestCase):
    def fixture(self):
        basic = ['TestBPSLiveCodexIntegration/'+n for n in ('file','resume','compact','search','mcp_parallel','candy_account_1','candy_account_2')]
        basic += ['TestBPSLiveCodexWebSocket/'+n for n in ('file','resume','compact')]
        basic += ['TestBPSLiveSharedAPIKeyCache','TestBPSLiveCodexWebSocket','TestBPSLiveCompatibleModelRoutes','TestBPSLiveStructuredOutput']
        basic += ['TestBPSLiveCompatibleModelRoutes/'+n for n in ('gpt-5.5','codex-auto-review','gpt-reserve','gpt-5.5-codex','unavailable-client-text-model')]
        supplementary=['TestBPSLiveCodexIntegration','TestBPSLiveCodexIntegration/apply_patch','TestBPSLiveInlineImageAttachment','TestBPSLiveToolInlineImageAttachment','TestBPSLiveStructuredCLI','TestBPSLiveStructuredCLI/official_schema_sse_tool_workflow','TestBPSLiveStructuredCLI/official_schema_ws_tool_workflow']
        case={'case':'apply_patch','passed':False,'cli':{'timed_out':False,'exit_code':1,'errors':['exceeded retry limit, last status: 429 Too Many Requests']},'gateway_metrics':[{'status':'error','http_code':429,'error_source':'upstream'}]}
        original='\n'.join('--- PASS: '+n+' (1s)' for n in basic)+'\n'+json.dumps(case,separators=(',',':'))+'\n--- FAIL: TestBPSLiveCodexIntegration (1s)\n--- FAIL: TestBPSLiveCodexIntegration/apply_patch (1s)\nFAIL\n'
        recovery='\n'.join('--- PASS: '+n+' (1s)' for n in supplementary)+'\nPASS\n'
        return original,recovery

    def test_complete_same_source_coverage_does_not_relabel_original_run(self):
        result=verify_logs(*self.fixture())
        self.assertTrue(result['passed'])
        self.assertFalse(result['original_run_passed'])

    def test_rejects_missing_unresolved_and_unrelated_failures(self):
        original,recovery=self.fixture()
        cases=[(original,recovery.replace('--- PASS: TestBPSLiveInlineImageAttachment (1s)\n','')),
               (original,recovery.replace('PASS: TestBPSLiveInlineImageAttachment','FAIL: TestBPSLiveInlineImageAttachment')),
               (original.replace('429 Too Many Requests','502 Bad Gateway'),recovery),
               (original.replace('"error_source":"upstream"','"error_source":"gateway"'),recovery),
               (original.replace('PASS: TestBPSLiveCodexIntegration/search','FAIL: TestBPSLiveCodexIntegration/search'),recovery),
               (original,recovery+'--- SKIP: TestUnexpected (1s)\n')]
        for pair in cases:
            with self.subTest(pair=cases.index(pair)),self.assertRaises(RuntimeError):verify_logs(*pair)


if __name__=='__main__':unittest.main()
