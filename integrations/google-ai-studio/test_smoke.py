"""Testes offline dos artefatos de integração Gemini."""
import json
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


class GeminiArtifactsTest(unittest.TestCase):
    def test_preset_protocol_and_no_secret(self):
        preset = json.loads((ROOT / "internal/provider/presets/gemini.json").read_text())
        self.assertEqual(preset["type"], "openai_compatible")
        self.assertEqual(preset["auth"]["scheme"], "bearer")
        self.assertEqual(preset["base_url"], "https://generativelanguage.googleapis.com/v1beta/openai")
        self.assertEqual(preset["limit_scope"], "provider")
        self.assertNotIn("AIza", json.dumps(preset))

    def test_mathlab_preserves_routes_and_adds_gemini(self):
        profile = json.loads((ROOT / "integrations/google-ai-studio/mathlab.project.json").read_text())
        for route in ("prover", "critic", "formalizer", "planner", "explorer", "summarizer", "gemini"):
            self.assertIn(route, profile["routes"])
        self.assertEqual(profile["keys"]["gemini"]["include"]["tags_any"], ["mathlab"])
        self.assertIn("REPLACE_WITH_MODEL_ID_FROM_KEY_TEST", profile["routes"]["gemini"]["chain"][0])

    def test_bootstrap_keeps_existing_providers(self):
        config = json.loads((ROOT / "configs/examples/keypool.json").read_text())
        self.assertTrue({"groq", "openrouter", "gemini"}.issubset(config["providers"]))


if __name__ == "__main__":
    unittest.main()
