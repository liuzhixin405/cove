#!/usr/bin/env python3
import argparse
import hashlib
import os
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from zipfile import ZipFile, ZIP_DEFLATED

TARGETS = [
    ("windows", "amd64", ".exe", "zip"),
    ("linux", "amd64", "", "tar.gz"),
    ("darwin", "amd64", "", "tar.gz"),
    ("darwin", "arm64", "", "tar.gz"),
]


def find_go_binary() -> str:
    candidates = [
        os.environ.get("GO_BIN", ""),
        shutil.which("go") or "",
        "/mnt/c/Program Files/Go/bin/go.exe",
        r"C:\Program Files\Go\bin\go.exe",
    ]
    for candidate in candidates:
        if candidate and Path(candidate).exists():
            return candidate
    raise FileNotFoundError("go executable not found; set GO_BIN or add Go to PATH")


def run(cmd, *, cwd=None, env=None):
    print("+", " ".join(cmd))
    subprocess.run(cmd, cwd=cwd, env=env, check=True)


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def zip_single(binary: Path, archive: Path, arcname: str):
    with ZipFile(archive, "w", compression=ZIP_DEFLATED) as zf:
        zf.write(binary, arcname=arcname)


def tar_gz_single(binary: Path, archive: Path, arcname: str):
    import tarfile
    with tarfile.open(archive, "w:gz") as tf:
        tf.add(binary, arcname=arcname)


def copy_latest_tree(src_dir: Path, latest_dir: Path):
    if latest_dir.exists():
        try:
            shutil.rmtree(latest_dir)
        except PermissionError as exc:
            print(f"Warning: could not fully replace {latest_dir}: {exc}")
            latest_dir.mkdir(parents=True, exist_ok=True)
            for src_path in sorted(src_dir.rglob('*')):
                rel = src_path.relative_to(src_dir)
                dst_path = latest_dir / rel
                if src_path.is_dir():
                    dst_path.mkdir(parents=True, exist_ok=True)
                    continue
                dst_path.parent.mkdir(parents=True, exist_ok=True)
                try:
                    shutil.copy2(src_path, dst_path)
                except PermissionError as exc:
                    print(f"Warning: could not update {dst_path}: {exc}")
            return
    shutil.copytree(src_dir, latest_dir)


def main():
    parser = argparse.ArgumentParser(description="Build release artifacts for cove")
    parser.add_argument("version", help="semantic version, with or without leading v")
    args = parser.parse_args()

    version = args.version if args.version.startswith("v") else f"v{args.version}"
    version_plain = version[1:]

    repo_root = Path(__file__).resolve().parents[1]
    dist_dir = repo_root / "dist" / version
    dist_dir.mkdir(parents=True, exist_ok=True)

    commit = os.environ.get("GIT_COMMIT", "unknown")
    try:
        commit = subprocess.check_output(["git", "rev-parse", "--short", "HEAD"], cwd=repo_root, text=True).strip()
    except Exception:
        pass
    build_time = datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")

    checksums = []
    go_bin = find_go_binary()

    for goos, goarch, exe_suffix, archive_kind in TARGETS:
        binary_name = f"cove-{version}-{goos}-{goarch}{exe_suffix}"
        binary_path = dist_dir / binary_name
        env = os.environ.copy()
        env.update({
            "GOOS": goos,
            "GOARCH": goarch,
            "CGO_ENABLED": "0",
        })
        ldflags = f"-X main.Version={version_plain} -X main.BuildTime={build_time} -X main.GitCommit={commit}"
        output_arg = os.path.relpath(binary_path, repo_root)
        run([
            go_bin, "build",
            "-ldflags", ldflags,
            "-o", output_arg,
            "./cmd/cove",
        ], cwd=repo_root, env=env)

        if archive_kind == "zip":
            archive_path = dist_dir / f"cove-{version}-{goos}-{goarch}.zip"
            zip_single(binary_path, archive_path, arcname=f"cove{exe_suffix}")
        else:
            archive_path = dist_dir / f"cove-{version}-{goos}-{goarch}.tar.gz"
            tar_gz_single(binary_path, archive_path, arcname="cove")

        checksums.append((archive_path.name, sha256_file(archive_path)))

    checksums_path = dist_dir / "checksums.txt"
    with checksums_path.open("w", encoding="utf-8") as f:
        for name, digest in checksums:
            f.write(f"{digest}  {name}\n")

    latest_dir = repo_root / "dist" / "latest"
    copy_latest_tree(dist_dir, latest_dir)

    print(f"Built release artifacts in {dist_dir}")
    print(f"Checksums written to {checksums_path}")


if __name__ == "__main__":
    main()
