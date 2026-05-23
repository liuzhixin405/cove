import io
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import scripts.release_build as release_build


class ReleaseBuildTests(unittest.TestCase):
    def test_copy_latest_tree_falls_back_when_rmtree_hits_permission_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            dist_dir = repo_root / "dist" / "v1.2.3"
            latest_dir = repo_root / "dist" / "latest"
            dist_dir.mkdir(parents=True)
            latest_dir.mkdir(parents=True)
            (dist_dir / "checksums.txt").write_text("new-checksums", encoding="utf-8")
            (dist_dir / "agentgo-v1.2.3-windows-amd64.exe").write_text("new-binary", encoding="utf-8")
            locked = latest_dir / "agentgo-v1.0.0-windows-amd64.exe"
            locked.write_text("old-binary", encoding="utf-8")

            copied = []
            original_copy2 = shutil.copy2

            def tracking_copy2(src, dst, *args, **kwargs):
                copied.append((Path(src).name, Path(dst).name))
                return original_copy2(src, dst, *args, **kwargs)

            def fake_rmtree(path):
                raise PermissionError("locked file")

            with mock.patch.object(release_build.shutil, "rmtree", side_effect=fake_rmtree):
                with mock.patch.object(release_build.shutil, "copy2", side_effect=tracking_copy2):
                    with mock.patch("sys.stdout", new_callable=io.StringIO) as stdout:
                        release_build.copy_latest_tree(dist_dir, latest_dir)

            self.assertTrue((latest_dir / "checksums.txt").exists())
            self.assertEqual((latest_dir / "checksums.txt").read_text(encoding="utf-8"), "new-checksums")
            self.assertTrue(any(name == "checksums.txt" for name, _ in copied))
            self.assertIn("Warning: could not fully replace", stdout.getvalue())

    def test_copy_latest_tree_skips_locked_destination_file_and_continues(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo_root = Path(tmp)
            dist_dir = repo_root / "dist" / "v1.2.4"
            latest_dir = repo_root / "dist" / "latest"
            dist_dir.mkdir(parents=True)
            latest_dir.mkdir(parents=True)
            src_locked = dist_dir / "agentgo-v1.2.4-windows-amd64.exe"
            src_locked.write_text("new-binary", encoding="utf-8")
            src_ok = dist_dir / "checksums.txt"
            src_ok.write_text("new-checksums", encoding="utf-8")
            dst_locked = latest_dir / "agentgo-v1.2.4-windows-amd64.exe"
            dst_locked.write_text("old-binary", encoding="utf-8")

            original_copy2 = shutil.copy2

            def flaky_copy2(src, dst, *args, **kwargs):
                if Path(dst).name == "agentgo-v1.2.4-windows-amd64.exe":
                    raise PermissionError("destination locked")
                return original_copy2(src, dst, *args, **kwargs)

            with mock.patch.object(release_build.shutil, "rmtree", side_effect=PermissionError("locked tree")):
                with mock.patch.object(release_build.shutil, "copy2", side_effect=flaky_copy2):
                    with mock.patch("sys.stdout", new_callable=io.StringIO) as stdout:
                        release_build.copy_latest_tree(dist_dir, latest_dir)

            self.assertEqual(dst_locked.read_text(encoding="utf-8"), "old-binary")
            self.assertEqual((latest_dir / "checksums.txt").read_text(encoding="utf-8"), "new-checksums")
            out = stdout.getvalue()
            self.assertIn("Warning: could not fully replace", out)
            self.assertIn("Warning: could not update", out)


if __name__ == "__main__":
    unittest.main()
