from __future__ import annotations

import pathlib
import re
import unittest


REPOSITORY_ROOT = pathlib.Path(__file__).resolve().parents[2]
VALIDATORS = {
    "internalCost": (
        "scripts/stage6-cost/validate_internal_cost_evidence.py",
        "publish_immutable_with_sha256(args.output, encoded)",
    ),
    "capacity": (
        "scripts/stage6-capacity/validate_capacity_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "desktop": (
        "scripts/stage6-desktop/validate_desktop_native_acceptance.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "incident": (
        "scripts/stage6-incident/validate_incident_exercise_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "operations": (
        "scripts/stage6-operations/validate_operations_exercise_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "penetration": (
        "scripts/stage6-penetration/validate_penetration_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "recovery": (
        "scripts/stage6-recovery/validate_recovery_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "residency": (
        "scripts/stage6-residency/validate_data_residency_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "slo": (
        "scripts/stage6-slo/validate_slo_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
    "workerSupplyChain": (
        "scripts/stage6-security/validate_worker_supply_chain_evidence.py",
        "publish_immutable_with_sha256(output, encoded)",
    ),
}
AUXILIARY_VALIDATORS = {
    "candidateBundleValidation": "scripts/stage6-candidate/validate_candidate_evidence_bundle.py",
    "documentationSource": "scripts/stage6-documentation/validate_documentation_set.py",
    "finalGAReview": "scripts/stage6-final/validate_final_ga_review.py",
    "operationsUISource": "scripts/stage6-operations/validate_operations_ui_matrix.py",
    "productionRotation": "scripts/stage6-rotation/validate_production_rotation_evidence.py",
}
DIRECT_OUTPUT_WRITE = re.compile(r"\boutput\.(?:write_bytes|write_text)\s*\(")


class CandidateReceiptImmutablePublisherTest(unittest.TestCase):
    def test_all_ten_candidate_receipts_use_an_immutable_publisher(self) -> None:
        self.assertEqual(len(VALIDATORS), 10)
        for receipt, (relative_path, publisher_call) in sorted(VALIDATORS.items()):
            with self.subTest(receipt=receipt):
                source = (REPOSITORY_ROOT / relative_path).read_text(encoding="utf-8")
                self.assertIn(publisher_call, source)
                self.assertIsNone(
                    DIRECT_OUTPUT_WRITE.search(source),
                    f"{receipt} writes its final output directly",
                )

    def test_candidate_and_source_gate_receipts_use_the_shared_immutable_publisher(self) -> None:
        self.assertEqual(len(AUXILIARY_VALIDATORS), 5)
        for receipt, relative_path in sorted(AUXILIARY_VALIDATORS.items()):
            with self.subTest(receipt=receipt):
                source = (REPOSITORY_ROOT / relative_path).read_text(encoding="utf-8")
                self.assertIn("publish_immutable_with_sha256(output, encoded", source)
                self.assertIsNone(
                    DIRECT_OUTPUT_WRITE.search(source),
                    f"{receipt} writes its final output directly",
                )


if __name__ == "__main__":
    unittest.main()
