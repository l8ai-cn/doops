import argparse
import json
import re
import subprocess
import sys


PRIVATE_FIELD = (r"[\w.-]*(?:token|password|passwd|secret|credential|signature|api[_-]?key|"
                 r"private[_-]?key|authorization|cookie|client-key-data)|sig")


def resource_name(value):
    label = r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?"
    if len(value) > 253 or not re.fullmatch(rf"{label}(?:\.{label})*", value):
        raise argparse.ArgumentTypeError("expected a Kubernetes resource name")
    return value


def namespace(value):
    if "." in value or len(value) > 63:
        raise argparse.ArgumentTypeError("expected a namespace name")
    return resource_name(value)


def public_stderr(raw):
    text = raw.decode("utf-8", errors="replace").strip()
    # Structured dumps and multiline private material may contain unlabelled values.
    private = re.search(
        rf"""(?im)-----BEGIN [\w ]*PRIVATE KEY-----|\b(?:values|manifest|notes|hooks|stringData)["']?\s*[:=]|"""
        rf"""\b(?:data|config|{PRIVATE_FIELD})["']?[ \t]*[:=][ \t]*(?:$|[{{\[|>])""", text)
    if private:
        text = text[:private.start()] + "[REDACTED]"
    text = re.sub(r"(?i)([a-z][a-z0-9+.-]*://)[^/\s@]+@", r"\1[REDACTED]@", text)
    text = re.sub(r"(?i)\b(?:bearer|basic)\s+\S+", "[REDACTED]", text)
    text = re.sub(rf"""(?im)\b({PRIVATE_FIELD})["']?[ \t]*[:=][ \t]*(?:"(?:\\.|[^"\\])*(?:"|$)|'(?:[^']|'')*(?:'|$)|[^\n]*)""",
                  r"\1=[REDACTED]", text)
    text = re.sub(rf"(?i)--({PRIVATE_FIELD})\s+\S+", r"--\1 [REDACTED]", text)
    return text[:2000]


def read_json(tool, *args):
    operation = " ".join((tool, *args[:2]))
    try:
        result = subprocess.run(["/app/doops-shell", "--argv", tool, *args], capture_output=True)
    except OSError as error:
        sys.exit(f"cannot start /app/doops-shell: {error.strerror}")
    reason = public_stderr(result.stderr)
    if result.returncode:
        print(f"{operation} failed: {reason or 'no diagnostic on stderr'}", file=sys.stderr)
        sys.exit(result.returncode if result.returncode > 0 else 128 - result.returncode)
    if reason:
        print(reason, file=sys.stderr)
    try:
        return json.loads(result.stdout)
    except (UnicodeError, ValueError):
        sys.exit(f"{operation} returned invalid JSON")


def report(collect):
    try:
        summary = collect()
    except (KeyError, TypeError, AttributeError, ValueError):
        # Exceptions from malformed native output can quote private data.
        sys.exit("unexpected diagnostic response structure")
    print(json.dumps(summary, ensure_ascii=False, separators=(",", ":")))
