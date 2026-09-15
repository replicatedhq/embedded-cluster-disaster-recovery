# Validation guide

## Local and CI checks

Run the same checks as CI:

```sh
make verify
make package VERSION=0.1.1
docker build --build-arg TARGETARCH=amd64 -t embedded-cluster-dr:test .
```

The test suite covers authentication, protocol idempotency, private restore
configuration handoff, manifest-last publication, retention ordering, stable
scheduling, Kubernetes settings publication, and clean Velero completion. Helm
lint and render verify that the chart includes Velero CRDs and node agent but
does not create a storage location or credentials before the operator opts in.

## Cloudflare R2 end-to-end test

Use a dedicated empty bucket and an R2 API token limited to read/write objects
in that bucket. Do not put credentials on a command line or in shell history.

1. Install a DR-enabled application release and open the Disaster Recovery page
   in the EC admin console.
   In a clean namespace, confirm the controller becomes Ready without a
   temporary-root chmod error. In its pod, verify
   `/var/run/embedded-cluster-dr/workflows` is owned by UID 65532 and has mode
   0700.
2. Enter the R2 S3 endpoint
   `https://<account-id>.r2.cloudflarestorage.com`, region `auto`, bucket,
   isolated prefix, and scoped access key credentials. Leave custom CA and proxy
   empty for the baseline test.
3. Save setup. Confirm the writable check succeeds, download the one-time
   recovery key, and keep it outside the cluster.
4. Create representative application state in every protected namespace and
   persistent volume. Record an application-level checksum or query result.
5. Create a manual backup. Confirm the operation reaches `succeeded`, the UI
   lists exactly one ready recovery point, and R2 contains both Velero objects
   and the encrypted EC archive/manifest pair.
6. Enable a short test schedule. Confirm the scheduler uses one stable operation
   ID while a backup is in progress or retrying and creates no duplicate ready
   point for the same scheduled instant.
   Replace the controller pod during a backup and confirm the operation retries
   or fails explicitly; an incomplete backup must not appear as a ready recovery
   point.
7. Exercise retention with a small count. Confirm an expired point disappears
   from the UI before its Velero backup and encrypted archive are deleted.
8. Destroy the test cluster. Keep only the installer for the exact backed-up
   application release, the R2 values, and the downloaded recovery key.
9. On a blank replacement host, run `sudo ./my-app restore`. Enter the same R2
   values in the extension restore wizard, select the ready point, and allow EC
   to rebuild the backed-up node-role topology.
10. Confirm the extension is installed by the release, the storage credentials
    remain active in the rebuilt cluster, and the backup schedule is paused.
11. Verify Kubernetes resources, every protected volume, and the recorded
    application-level checksum/query. Confirm the restore is not marked complete
    when Velero reports partial failure.
12. After application health is verified, resume a schedule explicitly. Test a
    normal application upgrade as a separate operation.

Repeat the restore once in headless mode with a root-owned mode-0600 lifecycle
configuration file. Use EC's development-only restore stops to inspect the
bootstrap, imported-state, runtime, and topology boundaries independently.

## Additional compatibility cases

- amd64 and arm64 bootstrap binaries embedded in the extension chart.
- Single-node and mixed controller/worker topology.
- R2 and a conventional AWS S3 bucket.
- A TLS-intercepting endpoint with a custom CA.
- An authenticated explicit proxy used by both the extension and Velero.
- Network interruption during upload, manifest publication, Velero restore, and
  retention cleanup, followed by retry with the same operation ID.
- Wrong recovery key, wrong bucket, incomplete manifest, mismatched release,
  and a Velero partial failure all fail closed without reporting recovery
  complete.
