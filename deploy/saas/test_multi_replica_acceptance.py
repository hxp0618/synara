from __future__ import annotations

import importlib.util
import pathlib
import unittest


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parents[1]
specification = importlib.util.spec_from_file_location(
    "multi_replica_acceptance",
    SCRIPT_DIR / "multi-replica-acceptance.py",
)
if specification is None or specification.loader is None:
    raise RuntimeError("multi-replica acceptance runner could not be loaded")
acceptance = importlib.util.module_from_spec(specification)
specification.loader.exec_module(acceptance)


class MultiReplicaWorkerManifestTest(unittest.TestCase):
    def test_builds_manifest_from_live_target_policy(self) -> None:
        capabilities = acceptance.build_worker_capabilities(
            REPO_ROOT / "scripts/stage3-provider-acceptance/worker_manifest.py",
            REPO_ROOT / "packages/contracts/src/providerCapabilityCatalog.json",
            {"providerPolicy": {"experimentalProviders": ["codex"]}},
            "acceptance",
        )

        providers = capabilities["providerHost"]["providers"]
        self.assertEqual(capabilities["workerRuntime"]["workerBuildVersion"], "acceptance")
        self.assertTrue(providers["codex"]["capabilityDescriptor"]["releasePolicy"]["enabled"])
        self.assertFalse(providers["claudeAgent"]["capabilityDescriptor"]["releasePolicy"]["enabled"])
        self.assertEqual(len(providers), 8)

    def test_rejects_missing_target_capabilities(self) -> None:
        with self.assertRaisesRegex(acceptance.AcceptanceError, "Target capabilities"):
            acceptance.build_worker_capabilities(
                REPO_ROOT / "scripts/stage3-provider-acceptance/worker_manifest.py",
                REPO_ROOT / "packages/contracts/src/providerCapabilityCatalog.json",
                None,
                "acceptance",
            )


if __name__ == "__main__":
    unittest.main()
