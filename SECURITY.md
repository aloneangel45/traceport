# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| 1.1.x   | Yes       |
| 1.0.x   | No        |
| < 1.0   | No        |

## Security Architecture

- Session identifiers are strictly validated (`^[a-zA-Z0-9_-]{1,128}$`) to prevent path traversal.
- Shadow-tree manifest paths reject absolute paths, parent traversal (`..`), NUL bytes, reserved directories (`.git`, `.traceport`), and directory symlink escapes.
- Content-addressed Zstandard blobs and manifests are verified against SHA-256 digests prior to workspace restoration (`traceport verify`).
- Legacy Git tree restoration validates object hashes (`^[0-9a-f]{40}([0-9a-f]{24})?$`) and isolates index operations via temporary `GIT_INDEX_FILE` paths.
- Installer scripts (`install.sh`, `install.ps1`) enforce SHA-256 verification against release `checksums.txt` manifests before binary extraction.

## Reporting a Vulnerability

If you discover a security vulnerability within TracePort:
1. Do NOT open a public issue.
2. Send an email to `security@aloneangel.dev` or open a confidential GitHub Security Advisory directly on this repository.
3. Include reproduction steps, proof-of-concept details, and affected versions.
