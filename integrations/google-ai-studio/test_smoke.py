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
        integration = json.loads((ROOT / "integrations/google-ai-studio/provider.json").read_text())
        limits = integration["limits"]
        self.assertEqual(limits["rpd_reset_timezone"], "America/Los_Angeles")
        self.assertEqual(limits["per_model"]["gemini-3.8-flash"], {"rpm": 5, "tpm": 250000, "rpd": 20})
        self.assertNotIn("AIza", json.dumps(preset))

    def test_generic_example_profile_routes_to_gemini(self):
        profile = json.loads((ROOT / "configs/examples/example.project.json").read_text())
        self.assertEqual(profile["project"], "example")
        self.assertEqual(profile["keys"]["gemini"]["include"]["tags_any"], ["example"])
        self.assertTrue(any(target.startswith("gemini:") for target in profile["routes"]["assistant"]["chain"]))

    def test_bootstrap_keeps_existing_providers(self):
        config = json.loads((ROOT / "configs/examples/keypool.json").read_text())
        self.assertTrue({"groq", "openrouter", "gemini"}.issubset(config["providers"]))
        self.assertEqual(config["providers"]["gemini"]["limits"]["rpd_reset_timezone"], "America/Los_Angeles")


if __name__ == "__main__":
    unittest.main()
