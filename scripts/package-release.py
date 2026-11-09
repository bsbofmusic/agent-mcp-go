#!/usr/bin/env python3
"""Build the validated Linux amd64 release with checksums and dependency notices."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def run(*args):
    return subprocess.check_output(args, cwd=ROOT, text=True)


def modules():
    pending = run("go", "list", "-m", "-json", "all").lstrip()
    decoder = json.JSONDecoder()
    while pending:
        module, end = decoder.raw_decode(pending)
        yield module
        pending = pending[end:].lstrip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag", help="release tag, for example v1.0.0")
    args = parser.parse_args()
    if not re.fullmatch(r"v\d+\.\d+\.\d+", args.tag):
        parser.error("tag must be vMAJOR.MINOR.PATCH")
    version = re.search(r'const version = "([^"]+)"', (ROOT / "main.go").read_text()).group(1)
    if args.tag != "v" + version:
        parser.error("tag must match main.go version")
    if run("go", "env", "GOOS", "GOARCH").split() != ["linux", "amd64"]:
        parser.error("the validated release target is native Linux amd64")
    run("go", "mod", "download", "all")
    notices = []
    for module in modules():
        if module.get("Main"):
            continue
        directory = Path(module["Dir"])
        licenses = sorted(f for f in directory.iterdir() if f.is_file() and f.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS")))
        if not licenses:
            raise SystemExit("Missing dependency license: " + module["Path"])
        notices.append("\n===== " + module["Path"] + " " + module["Version"] + " =====\n")
        for license_file in licenses:
            notices.append("\n--- " + license_file.name + " ---\n" + license_file.read_text() + "\n")
    output = ROOT / "dist"
    output.mkdir(exist_ok=True)
    archive = output / ("agent-mcp-go-" + args.tag + "-linux-amd64.tar.gz")
    files = ["README.md", "LICENSE", "NOTICE", "CHANGELOG.md", "config.example.json", "assets/CODEXFREE-REVIEW-LICENSE.txt", "docs/configuration.md", "docs/integrations.md", "docs/security.md", "docs/releases/" + args.tag + ".md", "systemd/agent-mcp-go.service"]
    with tempfile.TemporaryDirectory(prefix="amg-release-") as temp:
        binary = Path(temp) / "agent-mcp-go"
        env = dict(os.environ, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-buildvcs=false", "-trimpath", "-o", str(binary), "."], cwd=ROOT, env=env, check=True)
        payloads = {name: (ROOT / name).read_bytes() for name in files}
        payloads["agent-mcp-go"] = binary.read_bytes()
        payloads["VERSION"] = (version + "\n").encode()
        payloads["THIRD_PARTY_LICENSES.txt"] = "".join(notices).encode()
        with archive.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped, tarfile.open(mode="w", fileobj=zipped) as tar:
            for name, content in sorted(payloads.items()):
                info = tarfile.TarInfo(name)
                info.size = len(content)
                info.mode = 0o755 if name == "agent-mcp-go" else 0o644
                info.mtime = 0
                tar.addfile(info, io.BytesIO(content))
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    (output / "checksums.txt").write_text(digest + "  " + archive.name + "\n")
    print(archive.name + "  sha256=" + digest)


if __name__ == "__main__":
    main()
