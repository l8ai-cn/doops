#!/usr/bin/env python3
"""
不依赖 Docker/nerdctl 从零构建 OCI 镜像 tar
用法: python3 build_oci_image.py <binary_path> <output.tar>
然后在目标节点: ctr images import output.tar
     或: ctr images push <registry>/<image>:<tag> output.tar
"""
import sys
import os
import json
import hashlib
import tarfile
import io
import gzip
import time

def sha256(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()

def build_oci_tar(binary_path: str, output_tar: str, image_name: str = "doops-agent", image_tag: str = "latest"):
    print(f"[*] Reading binary: {binary_path}")
    with open(binary_path, "rb") as f:
        binary_data = f.read()
    binary_size = len(binary_data)
    print(f"    Size: {binary_size / 1024 / 1024:.1f} MB")

    # ── Layer: rootfs tar.gz ──────────────────────────────────────────
    print("[*] Building layer tar...")
    layer_tar_buf = io.BytesIO()
    with tarfile.open(fileobj=layer_tar_buf, mode="w") as lt:
        # /app directory
        dir_info = tarfile.TarInfo(name="app")
        dir_info.type = tarfile.DIRTYPE
        dir_info.mode = 0o755
        dir_info.mtime = int(time.time())
        lt.addfile(dir_info)

        # /app/doops-agent binary
        bin_info = tarfile.TarInfo(name="app/doops-agent")
        bin_info.size = binary_size
        bin_info.mode = 0o755
        bin_info.mtime = int(time.time())
        lt.addfile(bin_info, io.BytesIO(binary_data))

    layer_tar_bytes = layer_tar_buf.getvalue()

    # gzip compress the layer
    print("[*] Compressing layer...")
    gzip_buf = io.BytesIO()
    with gzip.GzipFile(fileobj=gzip_buf, mode="wb", mtime=0) as gz:
        gz.write(layer_tar_bytes)
    layer_gz = gzip_buf.getvalue()

    layer_digest = sha256(layer_gz)
    layer_size   = len(layer_gz)
    print(f"    Layer digest: {layer_digest[:32]}...")

    # ── Config JSON ───────────────────────────────────────────────────
    config = {
        "architecture": "amd64",
        "os": "linux",
        "config": {
            "Cmd": ["/app/doops-agent"],
            "Env": [
                "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
            ],
            "ExposedPorts": {"42222/tcp": {}},
            "WorkingDir": "/app"
        },
        "rootfs": {
            "type": "layers",
            "diff_ids": [sha256(layer_tar_bytes)]
        },
        "history": [{"created": "2026-01-01T00:00:00Z", "created_by": "doops-agent build"}]
    }
    config_bytes = json.dumps(config, separators=(",", ":")).encode()
    config_digest = sha256(config_bytes)
    config_size   = len(config_bytes)

    # ── OCI Manifest ──────────────────────────────────────────────────
    manifest = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "config": {
            "mediaType": "application/vnd.oci.image.config.v1+json",
            "digest": config_digest,
            "size": config_size
        },
        "layers": [{
            "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
            "digest": layer_digest,
            "size": layer_size
        }]
    }
    manifest_bytes = json.dumps(manifest, separators=(",", ":")).encode()
    manifest_digest = sha256(manifest_bytes)

    # ── OCI Index (top-level index.json) ──────────────────────────────
    index = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [{
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "digest": manifest_digest,
            "size": len(manifest_bytes),
            "annotations": {
                "io.containerd.image.name": f"{image_name}:{image_tag}",
                "org.opencontainers.image.ref.name": image_tag
            }
        }]
    }
    index_bytes = json.dumps(index, separators=(",", ":")).encode()

    # ── OCI layout ────────────────────────────────────────────────────
    oci_layout = json.dumps({"imageLayoutVersion": "1.0.0"}).encode()

    # ── Assemble final tar ───────────────────────────────────────────
    print(f"[*] Writing OCI image tar: {output_tar}")

    def add_bytes(tf, name, data):
        info = tarfile.TarInfo(name=name)
        info.size = len(data)
        info.mtime = 0
        tf.addfile(info, io.BytesIO(data))

    with tarfile.open(output_tar, "w") as tf:
        add_bytes(tf, "oci-layout", oci_layout)
        add_bytes(tf, "index.json", index_bytes)

        # blobs/sha256/<digest>
        blobs_dir = tarfile.TarInfo("blobs")
        blobs_dir.type = tarfile.DIRTYPE
        blobs_dir.mtime = 0
        tf.addfile(blobs_dir)

        sha_dir = tarfile.TarInfo("blobs/sha256")
        sha_dir.type = tarfile.DIRTYPE
        sha_dir.mtime = 0
        tf.addfile(sha_dir)

        add_bytes(tf, f"blobs/sha256/{config_digest.split(':')[1]}", config_bytes)
        add_bytes(tf, f"blobs/sha256/{layer_digest.split(':')[1]}", layer_gz)
        add_bytes(tf, f"blobs/sha256/{manifest_digest.split(':')[1]}", manifest_bytes)

    size_mb = os.path.getsize(output_tar) / 1024 / 1024
    print(f"[✓] OCI tar built: {output_tar} ({size_mb:.1f} MB)")
    print(f"    Image ref:     {image_name}:{image_tag}")
    print(f"    Manifest:      {manifest_digest}")
    print()
    print("导入命令 (在每个节点上运行):")
    print(f"  ctr -n k8s.io images import {os.path.basename(output_tar)}")
    print()
    print("推送到 CNB 制品库 (在导入后运行):")
    print(f"  ctr -n k8s.io images tag {image_name}:{image_tag} docker.cnb.cool/l8ai/ai/doops.sh:v1")
    print("  ctr -n k8s.io images push docker.cnb.cool/l8ai/ai/doops.sh:v1")
    print()
    print("说明:")
    print("  这个脚本只生成裸 doops-agent 网关兜底镜像，不包含 doagent AI 内核、AIO Sandbox、BuildKit 或 skills。")
    print("  标准 doops-agent 镜像请使用 agent/Dockerfile.sandbox 或 agent/Dockerfile 通过 BuildKit 构建。")

if __name__ == "__main__":
    binary  = sys.argv[1] if len(sys.argv) > 1 else "agent/doops-agent"
    out_tar = sys.argv[2] if len(sys.argv) > 2 else "/tmp/doops-agent-oci.tar"
    image   = sys.argv[3] if len(sys.argv) > 3 else "docker.cnb.cool/l8ai/ai/doops.sh"
    tag     = sys.argv[4] if len(sys.argv) > 4 else "v1"
    build_oci_tar(binary, out_tar, image, tag)
