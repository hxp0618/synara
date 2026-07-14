from __future__ import annotations

import copy
import json
import unittest

import worker_manifest


class WorkerManifestTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.catalog = json.loads(worker_manifest.default_catalog_path().read_text())

    def test_builds_complete_protocol_v21_manifest_from_shared_catalog(self) -> None:
        capabilities = worker_manifest.build_worker_capabilities(
            self.catalog,
            {"providerPolicy": {"experimentalProviders": ["codex", "claudeAgent"]}},
            worker_version="acceptance",
        )

        runtime = capabilities["workerRuntime"]
        self.assertEqual(runtime["workerBuildVersion"], "acceptance")
        self.assertEqual(runtime["workerProtocolMinimum"], 2)
        self.assertEqual(runtime["runtimeEventMaximum"], 2)
        provider_host = capabilities["providerHost"]
        self.assertEqual(provider_host["protocolVersion"], {"major": 2, "minor": 1})
        providers = provider_host["providers"]
        self.assertEqual(list(providers), [entry["provider"] for entry in self.catalog["providers"]])

        capability_ids = set(self.catalog["capabilityIds"])
        for provider, summary in providers.items():
            descriptor = summary["capabilityDescriptor"]
            self.assertEqual(set(descriptor["capabilities"]), capability_ids, provider)
            self.assertEqual(summary["runtimeEventVersions"], {"minimum": 2, "maximum": 2})
        self.assertTrue(providers["codex"]["capabilityDescriptor"]["releasePolicy"]["enabled"])
        self.assertFalse(providers["cursor"]["capabilityDescriptor"]["runtime"]["available"])

    def test_matches_target_experimental_provider_policy(self) -> None:
        capabilities = worker_manifest.build_worker_capabilities(
            self.catalog,
            {"providerPolicy": {"experimentalProviders": ["codex"]}},
            worker_version="acceptance",
        )
        providers = capabilities["providerHost"]["providers"]
        self.assertTrue(providers["codex"]["capabilityDescriptor"]["releasePolicy"]["enabled"])
        self.assertFalse(providers["claudeAgent"]["capabilityDescriptor"]["releasePolicy"]["enabled"])

    def test_rejects_catalog_capability_matrix_drift(self) -> None:
        catalog = copy.deepcopy(self.catalog)
        del catalog["providers"][0]["capabilities"][catalog["capabilityIds"][0]]
        with self.assertRaisesRegex(ValueError, "capability matrix"):
            worker_manifest.build_worker_capabilities(catalog, {}, worker_version="acceptance")

    def test_rejects_empty_worker_version(self) -> None:
        with self.assertRaisesRegex(ValueError, "worker version"):
            worker_manifest.build_worker_capabilities(self.catalog, {}, worker_version="  ")


if __name__ == "__main__":
    unittest.main()
