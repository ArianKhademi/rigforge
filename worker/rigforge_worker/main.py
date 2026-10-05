"""Worker entrypoint.

rigforge-worker                 consume jobs from Redis (the normal mode)
rigforge-worker run <video>     process one local file without any services
                                and write the outputs next to it; used for
                                the README sample and for timing runs
"""

from __future__ import annotations

import argparse
import functools
import json
import logging
import signal
import sys
import time
from pathlib import Path

from . import gltf
from .config import DEFAULT_MODEL_PATH, RIG_DIR, Config


def serve() -> None:
    import redis

    from .db import PostgresJobStore
    from .pipeline import process_video
    from .queue import JobQueue
    from .storage import ObjectStorage
    from .worker import Worker

    cfg = Config.from_env()
    # decode_responses: stream fields come back as str instead of bytes.
    client = redis.Redis.from_url(cfg.redis_url, decode_responses=True)
    storage = ObjectStorage(
        cfg.s3_endpoint, cfg.s3_region, cfg.s3_bucket, cfg.s3_access_key_id, cfg.s3_secret_access_key
    )
    worker = Worker(
        queue=JobQueue(client, cfg.consumer_name, cfg.queue),
        store=PostgresJobStore(cfg.database_url, events=client),
        handler=functools.partial(
            process_video, storage=storage, model_path=cfg.pose_model_path, work_dir=cfg.work_dir
        ),
        heartbeat_interval=cfg.heartbeat_interval,
    )

    # Kubernetes sends SIGTERM before killing a pod. Stop taking new jobs and
    # let the current one finish; if the pod is killed anyway, the job's
    # message stays pending and another worker reclaims it.
    def shutdown(signum, _frame):
        logging.getLogger("rigforge.worker").info("signal %s: finishing current job, then exiting", signum)
        worker.stop()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    worker.run_forever()


def run_local(args: argparse.Namespace) -> None:
    from .pipeline import CHARACTER_NONE, run

    source = Path(args.video)
    out_dir = Path(args.out or source.with_suffix("").name + "_out")
    out_dir.mkdir(parents=True, exist_ok=True)
    character = None
    if args.character != CHARACTER_NONE:
        path = RIG_DIR / "default_character.glb" if args.character == "default" else Path(args.character)
        character = gltf.read_glb(path.read_bytes())

    started = time.perf_counter()
    outputs = run(source, out_dir, character, Path(args.model), lambda stage, pct: None)
    total = round(time.perf_counter() - started, 3)
    (out_dir / "work.mp4").unlink(missing_ok=True)  # intermediate, not an output
    print(
        json.dumps(
            {"out": str(out_dir), "metadata": outputs.metadata, "timings": outputs.timings, "total_seconds": total},
            indent=2,
        )
    )


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s", stream=sys.stdout)
    parser = argparse.ArgumentParser(prog="rigforge-worker")
    sub = parser.add_subparsers(dest="command")
    local = sub.add_parser("run", help="process one local video file")
    local.add_argument("video")
    local.add_argument("--out", help="output directory (default: <video name>_out)")
    local.add_argument("--character", default="default", help='"default", "none", or a path to a .glb')
    local.add_argument("--model", default=str(DEFAULT_MODEL_PATH))
    args = parser.parse_args()
    if args.command == "run":
        run_local(args)
    else:
        serve()


if __name__ == "__main__":
    main()
