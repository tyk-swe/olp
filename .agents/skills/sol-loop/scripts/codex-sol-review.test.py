import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path

csr = SourceFileLoader(
    "codex_sol_review", str(Path(__file__).with_name("codex-sol-review"))
).load_module()


def msg(phase, text):
    return {"type": "agentMessage", "phase": phase, "text": text}


class PickMessage(unittest.TestCase):
    def test_partial_and_final_answers_are_kept_in_order(self):
        got = csr.pick_message(
            [
                msg("commentary", "looking"),
                msg("partial_answer", "VERDICT: CHANGES\nS1 finding"),
                msg("final_answer", "S2 finding"),
            ]
        )
        self.assertEqual(got, "VERDICT: CHANGES\nS1 finding\nS2 finding")

    def test_final_answer_alone(self):
        got = csr.pick_message([msg("final_answer", "VERDICT: APPROVE")])
        self.assertEqual(got, "VERDICT: APPROVE")

    def test_no_answer_phase_falls_back_to_last_message(self):
        got = csr.pick_message([msg("commentary", "only this")])
        self.assertEqual(got, "only this")

    def test_no_messages_fails_the_turn(self):
        with self.assertRaises(csr.TurnFailed):
            csr.pick_message([])


if __name__ == "__main__":
    unittest.main()
