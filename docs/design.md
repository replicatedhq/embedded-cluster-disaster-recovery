# Disaster-recovery extension design

## Boundary

Embedded Cluster discovers one vendor-selected lifecycle handler and speaks the
same `lifecycle.embeddedcluster.replicated.com/v1alpha1` HTTP protocol to its
pre-cluster executable and in-cluster Service. EC is intentionally independent
of Velero, object-store APIs, schedules, retention rules, and recovery UI.

The bootstrap listener is a mode-0600 Unix socket authenticated by a random
bearer token. The in-cluster listener verifies a bound service-account token
with the exact lifecycle audience and declared service-account identity.
Browser traffic reaches either listener only through an EC-owned authenticated
proxy.

## Backup transaction

1. EC exports a versioned opaque archive and creates a stable operation ID.
2. EC uploads the archive to the in-cluster handler.
3. The extension creates a Velero backup using the vendor's namespace and
   cluster-resource policy with file-system volume backup enabled.
4. A Velero `Completed` phase with any errors, or any partial/failed phase,
   fails the operation.
5. The extension encrypts and uploads EC's archive using age scrypt and the
   recovery key.
6. Expired recovery points are hidden, deleted through Velero, and removed from
   object storage according to retention.
7. A `ready` manifest is written last. Listing and restore ignore every point
   without a valid ready manifest.

An interrupted attempt is retried with the same operation ID. The Velero backup
name, object keys, and scheduled operation IDs are deterministic. Attempt
deadlines may be extended without changing the idempotency identity.

## Restore transaction

The bootstrap executable connects directly to object storage, lists only ready
manifests, downloads and decrypts the selected EC archive, and returns its
descriptor. EC verifies the digest, archive paths, signed license, cluster seed,
and exact application release before importing any host state.

Bootstrap storage credentials and the recovery key are never put in operation
status or generic restore metadata. They are staged in a mode-0600 file below
the private operation directory and exposed only through the bootstrap Unix
socket after the operation succeeds. EC holds the opaque JSON in a mode-0600
file while it rebuilds the cluster.

EC first installs cluster infrastructure and this release-selected extension,
but leaves the application namespace empty. For the in-cluster phase, operation
creation synchronously persists the opaque
configuration as Kubernetes Secrets, creates the stable Velero repository
password and backup storage location, applies proxy configuration, and waits
for the Velero rollout. Only then does the handler acknowledge `Create`, which
lets EC delete its protected copy safely. The restore operation then creates a
Velero Restore from the paired backup. EC then reconciles the exact backed-up
Helm release and marks recovery complete only after both steps succeed. Restoring
before chart reconciliation is required because Velero file-system restore does
not overwrite data in a PVC that already exists.

The replacement cluster runs the exact backed-up release. Normal upgrade logic
is intentionally unavailable until recovery completes.

## Storage layout

Object keys are scoped by the configured prefix:

```text
recovery-points/<uuid>/manifest.json
recovery-points/<uuid>/ec-state-v1.tar.gz.age
```

Velero owns its objects below the same bucket/prefix. The manifest contains the
paired Velero backup name and EC state object key. Recovery-point IDs and object
paths are validated before use.

Retention first changes the manifest from `ready` to `deleting`. It asks Velero
to delete the backup, removes the encrypted archive, and removes the deleting
manifest last. A failed cleanup therefore remains hidden and retryable.

## Configuration and scheduling

Dynamic settings are stored in the extension namespace. The configuration
Secret is published last so scheduled work cannot observe a setup that has not
finished configuring Velero. A separate proxy Secret feeds both the Velero
server and node agent. The stable `velero-repo-credentials` Secret is created
before the first backup and recreated from the recovery key before restore.

The extension evaluates a standard five-field cron expression. With no complete
recovery point, the initial scheduled backup is immediately due with a stable
ID. Later IDs derive from the first scheduled instant after the newest complete
point, so failures retry one operation instead of creating duplicates.

Restore retains active storage credentials but clears and pauses the schedule.

## Initial support and limits

- S3-compatible object storage, including Cloudflare R2.
- Explicit HTTPS endpoints, path-style addressing, custom CA bundles, and an
  explicit HTTP/HTTPS proxy.
- Velero file-system backup through the node agent; CSI snapshots are disabled.
- Browser and headless restore, multi-node topology rebuild, and retry of a
  failed application restore.
- One lifecycle extension instance and one object-storage location per cluster.
- No partial EC-only restore and no cross-release restore.
