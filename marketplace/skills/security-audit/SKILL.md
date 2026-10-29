---
name: security-audit
description: Security audit: scan deps, check vulnerabilities.
---

# Security Audit

Find real, exploitable problems in this repository and report them with evidence. Do not change code unless the user asks for fixes.

## 1. Scope

- Identify the languages, package managers and entry points (HTTP handlers, CLI args, file parsers, message consumers).
- Ask what is in scope if the repository is large: a service, a directory, or the pending diff.

## 2. Dependencies

Run the scanner for each ecosystem present; skip any that is not installed and say so.

```bash
# Go
govulncheck ./...
# Node
npm audit --omit=dev
# Python
pip-audit
# .NET
dotnet list package --vulnerable --include-transitive
# Rust
cargo audit
```

For each finding, check whether the vulnerable function is actually reachable before calling it critical.

## 3. Secrets

- Search for committed credentials: `rg -n -i '(api[_-]?key|secret|password|token)\s*[:=]'` and private-key headers (`BEGIN .*PRIVATE KEY`).
- Check config samples, test fixtures and CI files. Report the file and line only; never print the secret value.

## 4. Code review checklist

- **Injection**: SQL built by string concatenation, shell commands with user input, template injection.
- **Path traversal**: user-controlled paths joined without cleaning and a root check.
- **SSRF**: server-side fetches of user-supplied URLs without blocking internal addresses.
- **AuthN/AuthZ**: endpoints missing auth, object access without an ownership check (IDOR).
- **Deserialization**: untrusted input into pickle/BinaryFormatter/yaml.load or similar.
- **Crypto**: MD5/SHA1 for passwords, hard-coded keys, `math/rand` for tokens, disabled TLS verification.
- **Resource limits**: unbounded reads, missing timeouts, regex on untrusted input.

## 5. Report

For each issue give: severity (critical/high/medium/low), file:line, the concrete attack (input → effect), and the smallest fix. List what you could not check. Put confirmed issues before suspicions, and label suspicions as such.
