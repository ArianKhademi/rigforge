"""Object storage for the worker: the same S3 API the api uses, pointed at
Cloudflare R2 (or MinIO in dev). Unlike the browser, the worker holds bucket
credentials: it runs inside the cluster and reads and writes objects directly.
"""

from __future__ import annotations

from pathlib import Path

import boto3
from botocore.config import Config as BotoConfig
from botocore.exceptions import ClientError

from .errors import PermanentError

CONTENT_TYPES = {
    ".glb": "model/gltf-binary",
    ".bvh": "text/plain",
    ".mp4": "video/mp4",
    ".jpg": "image/jpeg",
}


class ObjectStorage:
    def __init__(self, endpoint: str, region: str, bucket: str, access_key_id: str, secret_access_key: str):
        self.bucket = bucket
        self.client = boto3.client(
            "s3",
            endpoint_url=endpoint,
            region_name=region,
            aws_access_key_id=access_key_id,
            aws_secret_access_key=secret_access_key,
            config=BotoConfig(
                s3={"addressing_style": "path"},
                retries={"max_attempts": 5, "mode": "standard"},
                # Same reason as in the api's S3 client: only send the newer
                # CRC checksums when an operation requires them (R2 compatibility).
                request_checksum_calculation="when_required",
                response_checksum_validation="when_required",
            ),
        )

    def download(self, key: str, dest: Path) -> None:
        """Stream an object to disk. download_file reads the body in chunks
        (and in parallel ranges for big objects), so a multi-gigabyte source
        video never sits in memory."""
        try:
            self.client.download_file(self.bucket, key, str(dest))
        except ClientError as exc:
            if exc.response.get("Error", {}).get("Code") in ("404", "NoSuchKey", "NotFound"):
                # The object is gone; no retry will bring it back.
                raise PermanentError(f"source object {key} does not exist") from exc
            raise

    def upload(self, src: Path, key: str) -> None:
        """Upload a file. upload_file streams from disk and switches to a
        multipart upload on its own for large files."""
        content_type = CONTENT_TYPES.get(src.suffix, "application/octet-stream")
        self.client.upload_file(str(src), self.bucket, key, ExtraArgs={"ContentType": content_type})
