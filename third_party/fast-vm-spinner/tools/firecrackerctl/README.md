# firecrackerctl

Rust lifecycle helper for the FastVM Firecracker prototype.

For the Proxmox-vs-Firecracker workflow and compatibility notes, see
[`docs/backend-flavors.md`](../../docs/backend-flavors.md).

The template model is rootfs-based:

- base images: `/opt/fastvm-microvm/images/*.ext4`
- snapshot templates: `/opt/fastvm-microvm/templates/*.ext4`
- instances: `/opt/fastvm-microvm/instances/<id>/rootfs.ext4`

Each run creates a per-instance rootfs from the selected source rootfs, so
clones can diverge safely. On XFS with reflink enabled this is a CoW reflink;
otherwise it falls back to a sparse full copy.

## Build

```bash
cargo build --release --manifest-path tools/firecrackerctl/Cargo.toml
```

## Run Plain Ubuntu

```bash
sudo firecrackerctl run --id ubuntu-dev --template ubuntu --vcpus 2 --mem-mib 1024
```

When `--host-cpus`/`HOST_CPUS` is omitted, `firecrackerctl` reads the
configured systemd slice cpuset (`qemu.slice` by default) and automatically
assigns a host CPU set. Explicit `--host-cpus` still overrides the scheduler.

## Snapshot Into A Template

```bash
sudo firecrackerctl snapshot --id ubuntu-dev --template plain-ubuntu
```

## Clone From A Template

```bash
sudo firecrackerctl clone plain-ubuntu build-1 --vcpus 16 --mem-mib 65536
```

## Check Storage

```bash
sudo firecrackerctl storage-info --install-dir /opt/fastvm-microvm
```

Fast clone storage reports `REFLINK_SUPPORTED=1` and
`ROOTFS_CLONE_METHOD=reflink`.

## Networking

Each instance still gets its own tap interface and `/30` guest network. The
host firewall uses shared rules instead of per-VM rules:

- `FORWARD -i fc+`
- `FORWARD -o fc+ -m conntrack --ctstate RELATED,ESTABLISHED`
- `POSTROUTING -s 192.168.0.0/16 -o <default-route-iface> -j MASQUERADE`

Individual VM stops remove only legacy per-VM rules if present. The shared
rules stay installed because other Firecracker VMs may still be running.

## Stop

```bash
sudo firecrackerctl stop --id build-1 --delete-instance
```

## List Templates

```bash
sudo firecrackerctl list-templates
```
