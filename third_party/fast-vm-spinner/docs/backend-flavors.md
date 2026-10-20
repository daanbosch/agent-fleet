# FastVM Backend Flavors

FastVM currently has two VM backends:

- **Proxmox FastVM**: the Proxmox-backed HTTP API implementation.
- **Firecracker microVMs**: the Firecracker-backed HTTP API implementation,
  implemented server-side using the host-local Rust `firecrackerctl` helper.

Both backends are exposed through the same `fastvm-api` HTTP contract, so the
same `fastvm` client binary can create, list, SSH to, snapshot, converge, and
destroy VMs. Backend choice is a server-side API configuration.

## Quick Comparison

| Area | Proxmox FastVM | Firecracker microVM |
| --- | --- | --- |
| Client command | `fastvm` | `fastvm` |
| Control plane | `fastvm-api` HTTP service | `fastvm-api` HTTP service backed by `firecrackerctl` |
| Template type | Proxmox VM template ID | server-side ext4 rootfs template name, exposed to clients as VMID-compatible numeric templates |
| Instance ID | numeric VMID, usually `60000-60999` | numeric VMID-compatible ID, usually `60000-60999` |
| Storage | Proxmox storage, e.g. `fastvm-nvme` | files under `/opt/fastvm-microvm` |
| SSH access | host SSH proxy, port usually equals VMID | host SSH proxy, port usually equals VMID |
| vCPU sizing | use for single large 64-120+ vCPU VMs; FastVM caps virtio NIC queues at 64 separately | upstream Firecracker limit is 32 vCPUs per microVM |
| CPU placement | automatic qemu.slice placement and vCPU thread pinning | automatic qemu.slice placement and Firecracker vCPU thread pinning |
| Snapshot | `fastvm snapshot --vm VMID` | `fastvm snapshot --vm VMID` |
| Destroy semantics | returns when destroy is queued; Proxmox purge continues asynchronously | returns after local microVM stop/delete completes |
| Best current fit | single very large compile VMs, Proxmox templates, shared API, named users, converge | default FastVM workflow, fast local clones, scale-out experiments |

## CPU Sizing

Choose the backend based on the shape of the workload:

- Use **Proxmox FastVM** when a user needs one large machine, for example a
  64-120+ core compile box with hundreds of GiB of RAM. FastVM preserves the
  requested VM core count, but caps Proxmox virtio `net0.queues` at 64 because
  Proxmox rejects larger queue counts.
- Use **Firecracker microVMs** when a user needs fast create/clone/destroy or
  can shard work across multiple VMs. Firecracker's upstream machine-config API
  restricts `vcpu_count` to the `1..32` range, so a single Firecracker microVM
  cannot be a 64-core or 120-core build host.

Firecracker is intentionally optimized for small, fast, isolated microVMs. Each
microVM has one VMM/device path plus vCPU threads, and high vCPU counts hit
contention instead of scaling like a full QEMU/KVM VM. Do not patch around this
limit locally for FastVM; route large single-VM requests to Proxmox or split the
work across multiple Firecracker microVMs.

References:

- Firecracker design docs: `https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md`
- Firecracker machine-config API: `https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/swagger/firecracker.yaml`

## User Workflow

### Proxmox FastVM

Use this when you want a single large VM or the normal shared Proxmox template
workflow.

```bash
export FASTVM_API=http://atl1-pc1-025:8090

fastvm create -u alice -c 1 --cores 16 -m 65536
fastvm list -u alice
fastvm ssh 60019
fastvm snapshot --vm 60019
fastvm destroy --vm 60019
```

This backend returns VMIDs and SSH proxy commands such as:

```text
ssh -p 60019 ubuntu@atl1-pc1-025
```

### Firecracker microVMs

Use this on a Firecracker-enabled host for fast lifecycle workflows up to 32
vCPUs per microVM. On `atl1-pc1-027`, `/opt/fastvm-microvm` is mounted on the
local NVMe device `/dev/nvme0n1`.

Run `fastvm-api` on that host. Firecracker is the default backend when
`FASTVM_BACKEND` is unset:

```bash
export FASTVM_SSH_HOST=atl1-pc1-027.hosts.telnyx.io
export FASTVM_FIRECRACKER_INSTALL_DIR=/opt/fastvm-microvm
export FASTVM_FIRECRACKER_DEFAULT_TEMPLATE=9013

fastvm-api
```

Point the same client at the Firecracker API:

```bash
export FASTVM_API=http://atl1-pc1-027.hosts.telnyx.io:8090

fastvm create -u alice -c 1 --cores 16 -m 65536
fastvm list -u alice
fastvm ssh 60000
fastvm snapshot --vm 60000
fastvm create -u race -c 4 -t 60000
fastvm converge -u race --keep 60001
fastvm destroy -u race
```

For more than 32 vCPUs of Firecracker capacity, create multiple microVMs and
split the workload:

```bash
fastvm create -u compile-shards -c 4 --cores 32 -m 65536 -t 9013
```

Each Firecracker instance gets its own rootfs file:

```text
/opt/fastvm-microvm/instances/<id>/rootfs.ext4
```

The rootfs file is attached as `/dev/vda` inside the guest. Multiple
Firecracker VMs can share `/dev/nvme0n1` safely because they use separate
rootfs files. Do not attach `/dev/nvme0n1` itself read-write to multiple
guests.

`firecrackerctl` remains available for host-local maintenance and debugging,
but normal users should use the `fastvm` client against the Firecracker
`fastvm-api`.

## Switching Between Flavors

Switching is a server/API choice. The client only needs to point `FASTVM_API`
at the desired backend host.

To use Proxmox:

1. Run `fastvm-api` with `FASTVM_BACKEND=proxmox`.
2. Point `FASTVM_API` at the Proxmox FastVM API host.
3. Use Proxmox template VMIDs for `-t/--template`.

To use Firecracker:

1. Run `fastvm-api` on the Firecracker host with `FASTVM_BACKEND` unset or set to `firecracker`.
2. Point `FASTVM_API` at the Firecracker FastVM API host.
3. Use the same `fastvm create/list/get/snapshot/destroy/converge` commands.

The base Firecracker template is configured server-side with
`FASTVM_FIRECRACKER_DEFAULT_TEMPLATE`, normally `9013`. User-created
Firecracker snapshots are exposed through the existing numeric template flow:
`fastvm snapshot --vm 60000` creates Firecracker template `60000`, and clients
can clone it with `fastvm create -t 60000`.

## Custom Templates

Users can create reusable templates by creating a VM, installing the packages or
source tree they want, and snapshotting that VM:

```bash
fastvm create -u alice -c 1 -t 9013 --cores 16 -m 65536
fastvm ssh 60000
# customize the VM, then exit
fastvm snapshot --vm 60000
fastvm create -u alice -c 1 -t 60000 --cores 16 -m 65536
```

The source VM must come from a FastVM security template. By default that is
template `9013`; operators can add more allowed base template IDs with
`FASTVM_SECURITY_TEMPLATE_IDS=9013,9020`. Snapshotting a VM that was not created
from a registered security template is rejected.

This keeps self-service templates usable without allowing insecure base images
to become shared FastVM templates. Do not bake host-specific secrets, enrolled
agent identity, SSH host keys, or one-time bootstrap tokens into custom
templates. The security packages and services should be present in the image;
runtime enrollment/configuration remains the responsibility of the FastVM
enrollment service.

Backend details:

- Proxmox custom templates are tracked in `templates.json` after a successful
  snapshot, so templates created from `9013` can be cloned and snapshotted
  again.
- Firecracker custom templates inherit the security marker into
  `/opt/fastvm-microvm/templates/<template>.env`, so later clones keep the same
  guardrail.

## Compatibility Status

The `fastvm` client is backend-compatible through the server-side API. No
client backend flag is required.

Security-agent and host-identity enrollment should be handled after clone by a
service with Vault access, not by baking secrets into Firecracker templates. See
[FastVM Enrollment Service](fastvm-enrollment-service.md).

Known differences:

- Firecracker API hosts must have `firecrackerctl`, Firecracker assets, and the
  NVMe-backed `/opt/fastvm-microvm` tree installed locally. Use XFS with
  reflink enabled for the store; ext4 works functionally but falls back to full
  rootfs copies and is much slower for create/clone.
- The Firecracker backend uses `root` as the default guest SSH user unless
  `FASTVM_FIRECRACKER_SSH_USER` is changed.
- Firecracker base templates are string-named rootfs images on the server, but
  client-visible custom templates remain numeric because the existing CLI
  accepts numeric `-t/--template` values.
- Firecracker networking uses per-VM tap devices named `fc*` with shared host
  firewall rules for `fc+` and `192.168.0.0/16`. Individual VM teardown leaves
  those shared rules installed so other microVMs keep network access.
- Firecracker guests should use the legacy `iptables` frontend. The current
  Firecracker CI kernel has xtables/NAT support but not nf_tables, so Docker's
  default bridge networking fails when Ubuntu's `iptables-nft` alternative is
  active. Security template `9013` pins `iptables` and `ip6tables` to legacy so
  `dockerd` can use its normal bridge/NAT configuration.

Validate Firecracker storage on the host with:

```bash
sudo /opt/fastvm-microvm/bin/firecrackerctl storage-info --install-dir /opt/fastvm-microvm
```

Fast clone storage reports `STORAGE_FSTYPE=xfs`, `REFLINK_SUPPORTED=1`, and
`ROOTFS_CLONE_METHOD=reflink`.

## Benchmark Notes

Recent 2 vCPU / 4 GiB NVMe-backed disk tests showed:

| Test | Firecracker | Proxmox FastVM | Faster |
| --- | ---: | ---: | --- |
| Random 4K sync writes | 14.78s, 4,434 IOPS | 29.28s, 2,238 IOPS | Firecracker ~2.0x |
| 32 small files + fsync | ~1,426 files/s | ~775 files/s | Firecracker ~1.8x |
| Sequential 1 GiB + fsync | 470 MiB/s | 730 MiB/s | Proxmox ~1.55x |

Interpretation: Firecracker currently looks better for compile-like
metadata/small-file/fsync workloads. Proxmox FastVM was faster for sustained
sequential writes in this benchmark.
