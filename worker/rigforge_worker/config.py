"""Worker configuration, all from environment variables (same names as the api)."""

from __future__ import annotations

import os
import socket
from dataclasses import dataclass
from pathlib import Path

from .queue import QueueConfig

PACKAGE_DIR = Path(__file__).resolve().parent
# worker/rig holds the skeleton definition and the bundled default character.
RIG_DIR = Path(os.environ.get("RIGFORGE_RIG_DIR", PACKAGE_DIR.parent / "rig"))
DEFAULT_MODEL_PATH = PACKAGE_DIR.parent / "models" / "pose_landmarker_heavy.task"


@dataclass(frozen=True)
class Config:
    database_url: str
    redis_url: str
    s3_endpoint: str
    s3_region: str
    s3_bucket: str
    s3_access_key_id: str
    s3_secret_access_key: str
    pose_model_path: Path
    work_dir: str | None  # parent for per-job temp dirs; None = system default
    consumer_name: str
    heartbeat_interval: float
    queue: QueueConfig

    @staticmethod
    def from_env() -> Config:
        def required(name: str) -> str:
            value = os.environ.get(name)
            if not value:
                raise SystemExit(f"config: {name} is required")
            return value

        defaults = QueueConfig()
        queue = QueueConfig(
            max_attempts=int(os.environ.get("JOB_MAX_ATTEMPTS", defaults.max_attempts)),
            backoff_seconds=tuple(float(s) for s in os.environ.get("JOB_BACKOFF_SECONDS", "60,300,900").split(",")),
            visibility_timeout_ms=int(os.environ.get("JOB_VISIBILITY_TIMEOUT_MS", defaults.visibility_timeout_ms)),
        )
        return Config(
            database_url=required("DATABASE_URL"),
            redis_url=os.environ.get("REDIS_URL", "redis://localhost:6379/0"),
            s3_endpoint=required("S3_ENDPOINT"),
            s3_region=os.environ.get("S3_REGION", "auto"),
            s3_bucket=os.environ.get("S3_BUCKET", "rigforge-dev"),
            s3_access_key_id=required("S3_ACCESS_KEY_ID"),
            s3_secret_access_key=required("S3_SECRET_ACCESS_KEY"),
            pose_model_path=Path(os.environ.get("POSE_MODEL_PATH", DEFAULT_MODEL_PATH)),
            work_dir=os.environ.get("RIGFORGE_WORK_DIR") or None,
            # Each worker needs a unique consumer name inside the group. The pod
            # hostname is unique per replica; the pid separates local processes.
            consumer_name=os.environ.get("WORKER_NAME", f"{socket.gethostname()}-{os.getpid()}"),
            heartbeat_interval=float(os.environ.get("JOB_HEARTBEAT_SECONDS", "15")),
            queue=queue,
        )
