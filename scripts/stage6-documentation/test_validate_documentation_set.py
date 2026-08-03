from __future__ import annotations

import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_documentation_set.py")
REQUIRED_DOCUMENTS = {
    "documentation-index": "all",
    "user-guide": "end-user",
    "administrator-guide": "tenant-administrator",
    "deployment-guide": "deployment-operator",
    "troubleshooting-guide": "support-operator",
}


class ValidateDocumentationSetTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.reset_payload()
        self.matrix = self.root / "matrix.json"

    def reset_payload(self) -> None:
        self.payload = {
            "schemaVersion": "synara.enterprise-documentation-matrix.v1",
            "documents": [],
        }
        for document_id, audience in REQUIRED_DOCUMENTS.items():
            path = self.root / f"{document_id}.md"
            path.write_text(f"# {document_id}\n\nmarker:{document_id}\n", encoding="utf-8")
            self.payload["documents"].append(
                {
                    "id": document_id,
                    "audience": audience,
                    "path": path.name,
                    "contains": [f"marker:{document_id}"],
                }
            )

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.matrix.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")
        return self.run_existing_matrix()

    def run_existing_matrix(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--repository-root",
                str(self.root),
                "--matrix",
                str(self.matrix),
                "--output",
                str(self.root / "receipt.json"),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_validates_source_set_without_declaring_release_verified(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))
        self.assertEqual(receipt["documentCount"], 5)
        self.assertGreater(receipt["documentByteCount"], 0)
        self.assertTrue(all(document["sizeBytes"] > 0 for document in receipt["documents"]))
        self.assertEqual(
            receipt["assessment"], "source-documentation-validated-not-release-verified"
        )
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_rejects_existing_receipt_without_modifying_it(self) -> None:
        receipt = self.root / "receipt.json"
        sidecar = self.root / "receipt.json.sha256"
        receipt.write_text("retained receipt\n", encoding="utf-8")
        sidecar.write_text("retained sidecar\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not already exist", result.stderr)
        self.assertEqual(receipt.read_text(encoding="utf-8"), "retained receipt\n")
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retained sidecar\n")

    def test_rejects_missing_document_or_marker(self) -> None:
        self.payload["documents"].pop()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("documentation inventory drifted", result.stderr)

        self.reset_payload()
        self.payload["documents"][0]["contains"] = ["missing marker"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("marker is missing", result.stderr)

    def test_rejects_broken_or_insecure_links(self) -> None:
        first = self.root / self.payload["documents"][0]["path"]
        first.write_text(first.read_text(encoding="utf-8") + "[missing](missing.md)\n")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("broken local link", result.stderr)

        first.write_text("# index\n\nmarker:documentation-index\n[bad](http://example.com)\n")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("non-HTTPS external link", result.stderr)

    def test_rejects_duplicate_matrix_fields_and_symlinked_matrix(self) -> None:
        encoded = json.dumps(self.payload, indent=2)
        duplicate = encoded.replace(
            "{",
            '{\n  "schemaVersion": "synara.enterprise-documentation-matrix.v1",',
            1,
        )
        self.matrix.write_text(duplicate + "\n", encoding="utf-8")
        result = self.run_existing_matrix()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)

        self.matrix.unlink()
        real_matrix = self.root / "real-matrix.json"
        real_matrix.write_text(json.dumps(self.payload) + "\n", encoding="utf-8")
        self.matrix.symlink_to(real_matrix.name)
        result = self.run_existing_matrix()
        self.assertEqual(result.returncode, 2)
        self.assertIn("non-symlink", result.stderr)

    def test_rejects_secret_document_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer documentation-validator-secret"
        document = self.root / self.payload["documents"][0]["path"]
        document.write_text(
            f"# documentation-index\n\nmarker:documentation-index\n{secret}\n",
            encoding="utf-8",
        )
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)


if __name__ == "__main__":
    unittest.main()
