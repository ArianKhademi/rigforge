# Running against Cloudflare R2

Everything in this repo was tested against MinIO, which speaks the same S3 API.
This is how to point it at a real R2 bucket and run the 2 GB resume test
there. Dashboard labels are as of late 2026 and may move around; the values
you are collecting are the same whatever the layout: an **account id**, an
**access key id** and a **secret access key**.

## 1. Account and R2 plan

1. Sign in at <https://dash.cloudflare.com> (create an account if needed).
2. In the left sidebar open **R2 Object Storage**. The first time, Cloudflare
   asks you to enable R2 and add a payment method, even for the free tier.
   The free tier is 10 GB-month of storage, 1 million Class A operations
   (writes) and 10 million Class B operations (reads) per month; egress is
   free. Nothing in this repo gets near those limits: the resume test writes
   2.1 GiB once and deletes it, using about 70 Class A operations.

## 2. Create the bucket

1. **R2 → Create bucket**.
2. Name: `rigforge-dev` (the default in every config file here; any name works
   if you set `S3_BUCKET` to it).
3. Location: **Automatic** is fine. Storage class: Standard. Create.

## 3. Find your account id and the S3 endpoint

Open the bucket, then **Settings**. The **S3 API** box shows the endpoint:

```
https://<ACCOUNT_ID>.r2.cloudflarestorage.com/rigforge-dev
```

The part before `/rigforge-dev` is `S3_ENDPOINT` (and `S3_PUBLIC_ENDPOINT`,
which is the same on R2). The 32-character hex string is your account id; it
is also shown on the R2 overview page.

## 4. Create an API token

1. On the R2 overview page open **Manage R2 API Tokens** (under the
   **{ } API** menu on newer dashboards), then **Create API token**.
2. Name: `rigforge-dev`.
3. Permissions: **Object Read & Write**.
4. Specify buckets: **Apply to specific buckets only → rigforge-dev**. (An
   account-wide token also works, but scoping it to the bucket limits what a
   leaked key could do.)
5. TTL: forever, or a date; client IP filtering: none. Create.
6. Copy the **Access Key ID** and the **Secret Access Key** now. The secret is
   shown once. If you lose it, roll the token and get a new pair.

## 5. Configure Rigforge

```bash
cp .env.example .env
```

Fill in:

```bash
S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
S3_PUBLIC_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com
S3_REGION=auto
S3_BUCKET=rigforge-dev
S3_ACCESS_KEY_ID=<access key id>
S3_SECRET_ACCESS_KEY=<secret access key>
S3_CREATE_BUCKET=false
```

`.env` is git-ignored. Never paste these values anywhere else.

## 6. CORS (only for the browser UI)

The web app PUTs parts and downloads outputs straight from the bucket, so the
browser needs the bucket's permission to do that. The CLI client and the
resume test do not: skip this step if you only want the test.

Bucket → **Settings → CORS policy → Edit**, paste the contents of
[`docs/r2-cors.json`](r2-cors.json), and add the origin the app will be served
from (for the docker-compose stack that is `http://localhost:8081`). The
`ExposeHeaders: ["ETag"]` line is the one that matters: without it the browser
cannot read the ETag of an uploaded part and the upload cannot complete.

## 7. Run it

```bash
make dev-r2                                            # the stack, with R2 as the bucket
LOG=docs/upload_resume_test_r2.log make resume-test    # the 2 GB test against R2
```

Expected: `RESULT: PASS` with 0 parts re-sent. On R2 the last check (part ETag
equals the MD5 of the bytes) may report "skipped" if R2's part ETags are not
MD5s; everything else is identical. Afterwards the test deletes the asset, so
the bucket is empty again.

For the Kubernetes overlays, the same values go into
`deploy/k8s/overlays/<overlay>/config.env` and `secret.env` (templates:
`config.env.example` and `../../base/secret.env.example`).

## If something fails

| Symptom | Likely cause |
| --- | --- |
| `SignatureDoesNotMatch` | `S3_ENDPOINT` includes the bucket name. It must be `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` (a trailing slash is tolerated and removed). |
| `AccessDenied` on create or PUT | The token is not scoped to this bucket, or is read-only. |
| `NoSuchBucket` | Bucket name mismatch; `S3_CREATE_BUCKET` is `false` on R2 on purpose. |
| Browser upload stalls at 0% with console CORS errors | Step 6 not done, or the app's origin is missing from the policy. |
| `InvalidPart` at complete | Should not happen; it means an ETag was recorded that the bucket does not have. Reupload the part by resuming. |
