"""Acceptance bookkeeping must not turn missing execution into a pass."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('alpha_acceptance', Path(__file__).with_name('alpha-acceptance.py'))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)

class AcceptanceSummaryTests(unittest.TestCase):
    def test_go_test_timeouts_are_extended_only_for_docker_layer(self):
        self.assertEqual(acceptance.go_test_timeout('docker'), '60m')
        for layer in ('unit', 'postgres', 'T01'):
            with self.subTest(layer=layer):
                self.assertEqual(acceptance.go_test_timeout(layer), '15m')

    def test_no_named_test_is_not_run(self):
        result = acceptance.summarize_events('docker', [{'Action': 'pass', 'Package': 'sandbox'}], 0)
        self.assertEqual(result['status'], 'not_run')

    def test_security_test_skip_blocks_docker_layer(self):
        result = acceptance.summarize_events('docker', [
            {'Action': 'pass', 'Test': 'TestOther'},
            {'Action': 'skip', 'Test': 'TestEvaluationJudgeRejectsCandidateVerdictForgery'},
        ], 0)
        self.assertEqual(result['status'], 'blocked_or_skipped')
        self.assertEqual(result['unexecuted_skips'], ['TestEvaluationJudgeRejectsCandidateVerdictForgery'])

    def test_parent_helper_skip_is_distinct_from_missing_prerequisite(self):
        result = acceptance.summarize_events('postgres', [
            {'Action': 'pass', 'Test': 'TestAgentQueueConcurrentProcessesRespectLimits'},
            {'Action': 'skip', 'Test': 'TestAgentQueueProcessWorker'},
        ], 0)
        self.assertEqual(result['status'], 'passed')
        self.assertEqual(result['parent_helper_skips'], ['TestAgentQueueProcessWorker'])
        self.assertEqual(result['unexecuted_skips'], [])

    def test_helpers_alone_cannot_pass(self):
        result = acceptance.summarize_events('postgres', [{'Action': 'skip', 'Test': 'TestAgentQueueProcessWorker'}], 0)
        self.assertNotEqual(result['status'], 'passed')

    def test_wrong_named_test_cannot_satisfy_matrix(self):
        result = acceptance.summarize_events('T01', [{'Action': 'pass', 'Test': 'TestOther'}], 0, matrix=True)
        self.assertEqual(result['status'], 'not_run')

    def test_failed_process_cannot_be_masked_by_prior_pass(self):
        result = acceptance.summarize_events('docker', [{'Action': 'pass', 'Test': 'TestOther'}], 1)
        self.assertEqual(result['status'], 'failed')

    def test_failure_diagnostics_keep_assertion_and_timeout_output(self):
        diagnostics = acceptance.format_failure_diagnostics('docker', [
            {'Action': 'output', 'Test': 'TestJudge', 'Output': '--- FAIL: TestJudge\ncontext deadline exceeded\n'},
        ], ['TestJudge'], 1)
        self.assertIn('TestJudge', diagnostics)
        self.assertIn('--- FAIL: TestJudge', diagnostics)
        self.assertIn('context deadline exceeded', diagnostics)

    def test_nonzero_process_without_fail_event_keeps_tail_output(self):
        diagnostics = acceptance.format_failure_diagnostics('docker', [
            {'Action': 'output', 'Test': 'TestJudge', 'Output': 'test started\n'},
            {'Action': 'output', 'Package': 'orchestration', 'Output': 'go test timed out\n'},
        ], [], 1)
        self.assertIn('test started', diagnostics)
        self.assertIn('go test timed out', diagnostics)

    def test_failure_diagnostics_are_bounded_and_redact_database_credentials(self):
        diagnostics = acceptance.format_failure_diagnostics('docker', [
            {'Action': 'output', 'Test': 'TestJudge',
             'Output': 'postgres://acceptance:private-password@127.0.0.1:5432/acceptance\n' + 'x' * 5000},
        ], ['TestJudge'], 1)
        self.assertIn('[case output truncated]', diagnostics)
        self.assertIn('postgres://<redacted>@', diagnostics)
        self.assertNotIn('private-password', diagnostics)
        self.assertLessEqual(len(diagnostics), acceptance.MAX_FAILURE_DIAGNOSTIC_CHARS_PER_CASE + 200)

if __name__ == '__main__':
    unittest.main()
