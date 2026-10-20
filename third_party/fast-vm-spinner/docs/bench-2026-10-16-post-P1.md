# fastvm warm-stack benchmark — 2026-10-16 (post P1: isolation + parity ops)

Measured on the `atl1-pc1-003` lab node, commit `c529075` (`feat/warm-snapshots`),
script: `scripts/bench-parity.sh` (committed; run on the API host, cleans up
after itself; all timings are host-side API wall clock — in-guest clocks are
invalid on restored clones because KVM clock payloads stay frozen).

## Environment

| | |
|---|---|
| Node | Supermicro, AMD EPYC 7713 64-core, 2.0 TiB RAM |
| Kernel / FS | Ubuntu 6.8.0-146-generic, XFS 67.5T (FIEMAP/FICLONE-friendly) |
| Firecracker | v1.15.1 (offline-staged) |
| Guest template | `warm-ubuntu-dev` — 4 GiB memory-resident Ubuntu 24.04 |
| Tenant churn | a second user (`revenue-coder`) was creating/destroying VMs during the run |

## Results

| operation | samples | wall time (median) |
|---|---|---|
| cold create (boot, disk clone, sshd) | 3 | 3.70 s |
| **warm launch** (restore 4 GiB memory-resident clone, prior lifetime intact) | 5 | **0.17 s** |
| warm launch ×8, concurrent (total for 8/8 serving) | 1 | 1.08 s |
| **warm snapshot** (4 GiB memory-dump + vmstate surgery, consumes source) | 3 | **0.83 s** |
| pause (VMM freeze, memory resident) | 3 | 0.002 s |
| resume (instant continue) | 3 | 0.002 s |
| exec round-trip (SSH channel, `fastvm exec`) | 3 | 0.249 s |
| expose (host port alloc + socat forward up) | 1 | 0.014 s |
| HTTP fetch through exposed port (host → guest service) | 1 | 0.019 s (HTTP 200) |

### Isolation acceptance — per-tap eBPF L3 lock (P1)

```
fastvm isolation probe (expects L3 lock enforced)
  PASS  DNS resolver 10.255.0.100:53           allow
  PASS  public internet 1.1.1.1:443            allow
  PASS  host sshd on the tap gw :22            drop
  PASS  host API on the tap gw :8090           drop
  PASS  fastvm API host (host fast net)        drop
  PASS  metadata 169.254.169.254               drop
  PASS  DNS resolution api.telnyx.com

probe summary: 7 pass, 0 fail
```

The lock enforces: source-address integrity, own-subnet + DNS-only resolver
egress, RFC1918/metadata/multicast drops, ephemeral-window-only reachability
to host ports, and no IPv6. Same user's VMs are isolated from each other too
(every VM gets its own L3 world — matches the sandbox platform's probe table).

## Comparison with the sandbox platform's published numbers (same 4 GiB class)

Sandbox bench (2026-10-15, Firecracker arm, nested-KVM droplet) vs fastvm
warm stack (bare-metal EPYC — environments differ, both honest):

| 4 GiB lifecycle | sandbox platform (FC, nested KVM) | fastvm warm stack (bare metal) |
|---|---|---|
| single restore | 2.1 s (lazy/demand-paged) | **0.17 s** (eager reflink, first touch ~0: 8.1 s to first-touch 512 MiB there) |
| 5 concurrent forks | 3.7–3.9 s | **1.08 s for 8** |
| snapshot creation | 6.8–7.8 s | **0.83 s** |
| exec round-trip | ~60 ms (in-guest vsock agent) | 249 ms (SSH channel; guest-agent project would match) |
| cold create | ~2–4 s (cloud image) | 3.70 s (reflink rootfs) |

## Known rough edge (documented, not hidden)

The first host-initiated TCP connection into a freshly restored clone can sit
in SYN-retransmission ~5–30 s on this node before the handshake commits;
every later connection is sub-millisecond. Bisect-proven not-the-L3-lock,
not-NAT, not-the-static-neighbor (wire shows the guest SYN-ACKing instantly).
Hygiene kicks and exec now retry inside their windows; steady state
unaffected.
