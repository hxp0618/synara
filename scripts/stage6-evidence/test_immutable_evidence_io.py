from __future__ import annotations

import hashlib
import pathlib
import stat
import sys
import tempfile
import unittest
from unittest import mock


SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common import immutable_evidence_io as MODULE  # noqa: E402


class ImmutableEvidenceIOTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_reads_one_stable_bounded_regular_file(self) -> None:
        source = self.root / "source.json"
        source.write_bytes(b'{"source":"stable"}\n')
        self.assertEqual(
            MODULE.read_stable_regular_file(source, label="source", maximum_bytes=1024),
            source.read_bytes(),
        )

        link = self.root / "source-link.json"
        link.symlink_to(source.name)
        with self.assertRaises(MODULE.ImmutableEvidenceIOError):
            MODULE.read_stable_regular_file(link, label="source", maximum_bytes=1024)

        empty = self.root / "empty.json"
        empty.write_bytes(b"")
        with self.assertRaises(MODULE.ImmutableEvidenceIOError):
            MODULE.read_stable_regular_file(empty, label="source", maximum_bytes=1024)

        oversized = self.root / "oversized.json"
        oversized.write_bytes(b"x" * 1025)
        with self.assertRaises(MODULE.ImmutableEvidenceIOError):
            MODULE.read_stable_regular_file(oversized, label="source", maximum_bytes=1024)

    def test_publishes_private_receipt_once(self) -> None:
        output = self.root / "receipt.json"
        encoded = b'{"receipt":"validated"}\n'
        MODULE.publish_immutable_with_sha256(output, encoded)
        sidecar = output.with_suffix(".json.sha256")
        self.assertEqual(output.read_bytes(), encoded)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)
        self.assertEqual(
            sidecar.read_text(encoding="utf-8"),
            f"{hashlib.sha256(encoded).hexdigest()}  {output.name}\n",
        )
        with self.assertRaises(MODULE.ImmutableEvidenceIOError):
            MODULE.publish_immutable_with_sha256(output, b'{"replacement":true}\n')
        self.assertEqual(output.read_bytes(), encoded)

    def test_rolls_back_receipt_if_sidecar_publication_fails(self) -> None:
        output = self.root / "rollback.json"
        sidecar = output.with_suffix(".json.sha256")
        real_link = MODULE.os.link

        def fail_sidecar(source: pathlib.Path, destination: pathlib.Path, **kwargs: object) -> None:
            if pathlib.Path(destination) == sidecar:
                raise OSError("simulated sidecar failure")
            real_link(source, destination, **kwargs)

        with mock.patch.object(MODULE.os, "link", side_effect=fail_sidecar):
            with self.assertRaises(MODULE.ImmutableEvidenceIOError):
                MODULE.publish_immutable_with_sha256(output, b'{"receipt":"validated"}\n')
        self.assertFalse(output.exists())
        self.assertFalse(sidecar.exists())


if __name__ == "__main__":
    unittest.main()
