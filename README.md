# Embedded Cluster Disaster Recovery

This is the opt-in disaster-recovery lifecycle extension for Replicated
Embedded Cluster. A vendor includes this extension in an application release;
Embedded Cluster does not install Velero or expose disaster-recovery UI unless
the release selects a lifecycle handler.

The extension owns:

- Velero `v1.18.2`, the AWS object-store plugin `v1.14.2`, and file-system
  volume backup through the Velero node agent.
- S3-compatible storage configuration, including Cloudflare R2, custom CAs,
  explicit proxies, a schedule, and retention.
- The admin-console setup and backup UI and the pre-cluster restore wizard.
- Pairing the application backup with EC's opaque state archive and publishing
  a recovery point only after both succeed.
- Application restore, progress, retry behavior, and cleanup of expired points.

Embedded Cluster owns the exact-release check, its versioned state archive,
replacement-cluster creation, topology validation, and the generic lifecycle
transport. The extension never reads or changes the contents of EC's archive.

## Vendor opt-in

The application release includes the extension chart and bootstrap artifacts,
then binds backup and restore to the same handler:

```yaml
apiVersion: embeddedcluster.replicated.com/v1beta1
kind: Config
spec:
  extensions:
    helmCharts:
      - chart:
          name: embedded-cluster-disaster-recovery
          chartVersion: 0.1.0
        releaseName: embedded-cluster-disaster-recovery
        namespace: embedded-cluster-dr
    lifecycleHandlers:
      - name: replicated-disaster-recovery
        extensionChart: embedded-cluster-disaster-recovery
        bootstrap:
          artifacts:
            - os: linux
              arch: amd64
              path: embedded-cluster-dr-linux-amd64.tar.gz
              sha256: <release artifact sha256>
              executable: embedded-cluster-dr
            - os: linux
              arch: arm64
              path: embedded-cluster-dr-linux-arm64.tar.gz
              sha256: <release artifact sha256>
              executable: embedded-cluster-dr
        service:
          namespace: embedded-cluster-dr
          name: embedded-cluster-dr
          port: 8080
          serviceAccount: embedded-cluster-dr
  lifecycle:
    backup:
      handler: replicated-disaster-recovery
    restore:
      handler: replicated-disaster-recovery
  backup:
    includedNamespaces: [my-application]
    includedClusterResources: []
    volumeBackup: fileSystem
```

If the lifecycle fields are absent, the extension need not be included and no
backup or restore capability is required. Vandoor separately gates releases
that declare lifecycle backup, restore, or Velero usage.

## Operator workflow

After installation, the EC admin console proxies the extension's authenticated
setup UI. The operator supplies the endpoint, bucket, region, optional prefix,
access credentials, optional custom CA and proxy, schedule, and retention. The
extension verifies that storage is readable and writable before saving it.

On first setup, a recovery key is generated and offered for download exactly
once. It encrypts the EC state archive and is also the stable Velero repository
password required to reopen file-system backups on a replacement cluster. It
must be stored outside the cluster.

Restore starts from the installer for the exact release captured by the
recovery point:

```sh
sudo ./my-app restore
```

For headless restore, the extension-owned JSON input is read from an
owner-readable file:

```json
{
  "storage": {
    "bucket": "recovery",
    "prefix": "my-app",
    "region": "auto",
    "endpoint": "https://ACCOUNT.r2.cloudflarestorage.com",
    "accessKeyId": "REDACTED",
    "secretAccessKey": "REDACTED",
    "forcePathStyle": true,
    "customCaPem": "",
    "proxyUrl": ""
  },
  "recoveryKey": "REDACTED"
}
```

The same opaque configuration is transferred into the rebuilt cluster and
stored as Kubernetes Secrets. Scheduled backups are deliberately paused after
restore. The operator verifies the application, resumes a schedule explicitly,
and upgrades only as a separate later operation.

## Development

Requirements are Go 1.24+, Helm, and Docker.

```sh
make verify
make package VERSION=0.1.0
docker build --build-arg TARGETARCH=amd64 -t embedded-cluster-dr:dev .
```

`make package` produces Linux bootstrap archives for amd64 and arm64, the Helm
chart, and checksums. A `vX.Y.Z` tag publishes the multi-architecture image,
chart, bootstrap archives, checksums, and GitHub release.

See [design.md](docs/design.md) for invariants and
[testing.md](docs/testing.md) for the R2 end-to-end validation procedure.
