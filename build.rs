//! Build identity (0033): a hash of the inputs that decide runtime behaviour, so
//! traces, sessions and acceptance reports can tell one build of cove from another
//! without git. `COVE_SOURCE` is the first 12 hex digits of a sha256 over every
//! file under `src/`, `assets/` and `marketplace/` plus `Cargo.toml` and
//! `Cargo.lock`, in path order; docs and tests are left out because they do not
//! change what the binary does. `COVE_BUILT` is the build time in Unix seconds.

use std::path::{Path, PathBuf};

use sha2::{Digest, Sha256};

const DIRS: [&str; 3] = ["src", "assets", "marketplace"];
const FILES: [&str; 2] = ["Cargo.toml", "Cargo.lock"];

fn main() {
    let root = PathBuf::from(std::env::var_os("CARGO_MANIFEST_DIR").expect("CARGO_MANIFEST_DIR"));
    let mut files = Vec::new();
    for dir in DIRS {
        println!("cargo:rerun-if-changed={dir}");
        collect(&root, &root.join(dir), &mut files);
    }
    for file in FILES {
        println!("cargo:rerun-if-changed={file}");
        if root.join(file).is_file() {
            files.push(file.to_owned());
        }
    }
    files.sort();
    let mut hasher = Sha256::new();
    for name in &files {
        let data = std::fs::read(root.join(name)).expect("read source file");
        // Name and length delimit each file: moving bytes between files changes the hash.
        hasher.update(name.as_bytes());
        hasher.update([0]);
        hasher.update((data.len() as u64).to_le_bytes());
        hasher.update(&data);
    }
    let digest = hasher.finalize();
    let source: String = digest[..6]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    let built = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_secs());
    println!("cargo:rustc-env=COVE_SOURCE={source}");
    println!("cargo:rustc-env=COVE_BUILT={built}");
}

/// Files under `dir`, as `/`-separated paths relative to `root`.
fn collect(root: &Path, dir: &Path, files: &mut Vec<String>) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(kind) = entry.file_type() else {
            continue;
        };
        if kind.is_dir() {
            collect(root, &path, files);
        } else if kind.is_file()
            && let Ok(relative) = path.strip_prefix(root)
        {
            let parts: Vec<_> = relative
                .components()
                .map(|part| part.as_os_str().to_string_lossy().into_owned())
                .collect();
            files.push(parts.join("/"));
        }
    }
}
