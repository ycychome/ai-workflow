"""Acceptance bookkeeping must not turn missing execution into a pass."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('alpha_acceptance', Path(__file__).with_name('alpha-acceptance.py'))
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)

class AcceptanceSummaryTests(unittest.TestCase):
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

if __name__ == '__main__':
    unittest.main()
