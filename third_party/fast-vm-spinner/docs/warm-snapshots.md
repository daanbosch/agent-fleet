# Warm snapshot templates

Warm templates freeze a **running** microVM (memory + merged disk) and restore
clones with **no guest boot at all**:

| Launch method | Time to a running, SSH-able VM |
|---|---|
| Cold template (default image) | 3.5–5 s |
| Cold template with pre-baked SSH host keys | ~2.8 s |
| **Warm template (this feature)** | **~0.6 s** |

The restored clone is the *same machine*: uptime, running processes and
listening sockets continue from the moment the source was frozen. `sshd`
answers immediately — no race with boot.

- Firecracker backend only (warms use `PATCH /vm` → `PUT /snapshot/create`
  → per-clone `PUT /snapshot/load` + `network_overrides`, FC ≥ 1.13
  recommended / validated on 1.15.1).
- Templates live under `<installDir>/templates-warm/<name>/`
  (`vmstate.snap`, `mem.img`, `rootfs.ext4`, `meta.json`).

## Create your own warm template (with your SSH key baked in)

**Key rule:** warm clones do *not* get a key injected at launch (there is no
cloud-init pass — nothing boots). Whoever should be able to `ssh` into clones
must have their public key in the source VM's `authorized_keys` **before** you
freeze it.

```bash
# 1. Create a base VM from any cold template (3-5 s)
fastvm create -u $USER -c 1 --name mywarmbase -t <cold-template-id> --cores 2 -m 2048

# 2. SSH in and bake everything you want clones to start with:
#    your SSH key, dotfiles, warm caches, running services...
ssh -p 60000 ubuntu@<node>
  ssh-import-id gh:<your-github-handle>     # or: curl -s https://github.com/<you>.keys >> ~/.ssh/authorized_keys
  # ... ri, docker, preloaded models, whatever you want clones to wake up with
exit

# 3. Freeze it. The source VM is consumed (stopped + unregistered).
fastvm warm-snapshot --vm mywarmbase --name dev24

# 4. Restore clones whenever you want — one at a time (see rules below)
fastvm warm-launch -u $USER -t dev24
ssh -p <vmid> ubuntu@<node>
```

From then on your morning VM is `fastvm warm-launch -u $USER -t dev24` —
sub-second, exactly as you left it.

## Rules and constraints

- **One live clone per warm template.** The guest's IP and MAC are frozen
  inside the memory image; the host cannot route two identical subnets. A
  second `warm-launch` while a clone is live fails fast. Destroy the running
  clone first (`fastvm destroy --vm <id>`), then launch again.
- **Clones see the template's secrets.** A warm snapshot contains live guest
  memory — anything the source VM had in RAM or on disk is inherited by every
  clone: SSH host keys, shell history, tokens in your dotfiles, service
  state. Treat warm templates as *user-owned disposable dev images*:
  - never build a warm template from a VM holding production or team secrets;
  - warm templates are **never security-enrolled** (the API refuses to treat
    them as security sources);
  - prefer one warm template per person — do not share them.
- **A re-launch is a factory reset for on-disk changes**: clones get a fresh
  reflink copy of the template disk, so files written inside a destroyed
  clone do not carry over. Persist anything you want outside the template.
- `--ssh-key` on `create` has no launch-time equivalent effect for **old**
  templates; `warm-launch` accepts `-k/--ssh-key` (see below).

## Multiple clones: eBPF identity NAT

A warm template freezes the guest's own IP, its NIC MAC, its gateway IP *and*
its gateway MAC into the memory image — the kernel can route only one live
instance of that identity. Warm launches therefore use a per-clone **eBPF
identity NAT** (`internal/fastvm/bpf/warmnat.bpf.c`, attached via TCX and
pinned so it survives an API restart):

- every clone's tap gets a unique /30 from `FASTVM_WARM_NET`
  (default `172.29.0.0/16`);
- the tap carries the frozen gateway MAC and a static neighbor entry pins
  the unique clone IP to the frozen guest MAC, so the host itself never ARPs;
- the tcx/egress hook rewrites host→guest destination IPs (clone IP → frozen
  guest IP) and the tcx/ingress hook rewrites guest→host source IPs (frozen
  guest IP → clone IP), maintaining IPv4/TCP/UDP checksums incrementally;
- the ingress hook doubles as an **ARP responder**, answering the guest's
  probes for its frozen gateway with the frozen gateway MAC — no netns, no
  guest-side state;
- one already-live template can support **many concurrent clones** with
  independent disk/memory state.

The legacy single-clone behaviour remains reachable with
`FASTVM_WARM_IDENTITY_NAT=0` (frozen-identity tap, one live clone per
template).

## Per-launch hygiene (keys and clock)

- `fastvm warm-launch -k/--ssh-key PATH` appends that public key to the
  clone's root `authorized_keys` right after restore — the clone never
  boots, so this is a plain SSH append into the live guest.
- New snapshots request the Firecracker **Realtime clock state**
  (`clock_type=Realtime`) so restores can advance the guest clock with the
  native `clock_realtime` load option. Templates created before this field
  exist restore with frozen clocks (the guest believes time stopped at the
  freeze; the API detects this automatically and respawns the VMM without
  the clock option).

## CLI commands

```
fastvm warm-snapshot --vm VMID --name NAME   # freeze a running VM (source is consumed)
fastvm warm-launch -u USER -t NAME [-n NAME]# restore a clone (~0.6 s)
fastvm warm-templates                       # list available warm templates
```

## Under the hood (for maintainers)

- `internal/fastvm/warm.go` — engine, CLI, handlers, registry.
- The FC vmstate (`.snap`) file is **bitcode + trailing CRC64** (CRC-64/Jones,
  the `crc64` crate; `crc64(0, whole file) == 0`). Restoring a clone patches
  the embedded rootfs path in place (**equal length only** — all instance dirs
  are `instances/<5-digit-VMID>/`) and rewrites the trailing CRC. The tap is
  *not* patched; restore uses the official `network_overrides` field.
- The instance dir holds `vmstate.snap`, `mem.img`, `rootfs.ext4` — the disk
  is a reflink copy per clone (XFS), the memory file is restored read-only.
- `warmCleanupInstance` stops the transient `fastvm-microvm-<vmid>.service`
  systemd unit (cold-created sources), kills the VMM, removes the tap and the
  instance dir, and unregisterVMs the source.
- `internal/fastvm/warmnat.go` + `internal/fastvm/bpf/warmnat.{bpf.c,o}` —
  eBPF identity NAT: constants rewritten per-clone at load (cilium/ebpf
  `spec.Variables`), TCX ingress/egress attach, pinned links under
  `/sys/fs/bpf/fastvm-warm/<tap>/`, masquerade + forwarding rules for the
  clone range.

## Profiling: where launch/snapshot time goes

`fastvm profile` (or `GET /api/v1/profile`) prints per-stage breakdowns of
the last cold creates, warm snapshots and warm launches; `FASTVM_PPROF=1`
mounts Go's pprof endpoints under `/debug/pprof/` for daemon-level profiling.

Droplet reference numbers (4 GiB templates, nested KVM, DO network block
storage):

- cold create ~3.6-4.0 s: 86% is waiting for the guest to boot and answer SSH
  (`ctl_ssh_ready`); the reflink clone itself is ~20-30 ms. Post-boot extras
  (launch-key install, SSH proxy spawn) ~290 ms.
- warm snapshot ~0.6 s for 4 GiB: Firecracker fsyncs snapshot files by
  default, which caps the dump at the disk's sync-write rate (~570 MB/s on
  DO block storage); fastvm sets sync_snapshot_files=false so the dump
  lands in the page cache and the kernel flushes it in the background.
  FASTVM_WARM_SYNC=1 restores the old guarantee.
- clones do NOT copy the 4 GiB memory image: Firecracker restores straight
  from the template's mem.img (mapped private, demand-paged, shared page
  cache — one file on disk, one copy in cache, N live clones). A per-clone
  reflink of a fresh dump also stalls behind XFS flushing its dirty pages
  (FICLONE waits), so sharing is both cheaper and faster.
- warm launch (full identity-NAT path) ~130-145 ms median:
  reflink ~30 ms, tap+neigh ~30 ms, BPF attach ~20 ms, FC restore+resume
  ~45 ms, SSH proxy ~12 ms. A custom launch key (`-k`) and the clock kick
  run asynchronously after the response (allow ~1 s before the first SSH
  with the new key).


## Guest clock on restore

FC 1.17 snapshot files carry no KVM clock payload (that version has no
`clock_type` field at create), so a restored guest's CLOCK_REALTIME stays
at the freeze time. fastvm kicks it back to the host's time over the same
asynchronous post-restore SSH session that appends the launch key: within
roughly a second of launch the guest clock matches the host (verified with
a one-hour-old template: 1 s drift). The guest's uptime counter stays
frozen — that is inherent to resume-from-snapshot on this VMM version, and
matches what resumeinside a paused VM means on every platform (the sandbox
stack pushes fresh min/real clocks over its vsock agent for the same
reason).


## Do warm clones need userfaultfd? (measured: no)

The sandbox-stack benchmark shows CLH beating FC 8:1 on a "512 MiB page
touch after lazy restore" (0.62 s vs 8.1 s), which suggests Firecracker's
lazy restore needs a userfaultfd handler. Measured host-side on this stack,
the gap does not exist — it is a measurement artifact:

- benchmark setup: source VM writes a 512 MiB blob (snapshot captures it),
  warm-snapshot, warm-launch, then read the blob in the clone with the
  guest page cache dropped (every page faults the snapshot memory in).
- host-side wall time ~1 s for the full 512 MiB — parity with a cold-booted
  VM reading its own warm page cache. sha256 matches the pre-snapshot
  blob: lazy faults are both fast and integrity-preserving.
- in-guest timing on restored clones is UNRELIABLE and produced the 8-9 s
  figures: kvmclock re-anchoring after resume makes in-guest wall deltas
  jump erratically. Host-side timing (ssh wall time) is the only valid
  clock for warm-clone benchmarks.
- the shared template mem file lives in the host page cache after the
  (fsync-free) dump, so kernel COW faults resolve at RAM speed.

Firecracker 1.17 does ship the `Uffd` memory backend (grep the binary), so
an external userfaultfd sidecar is implementable later (e.g. for remote or
compressed memory), but it would close no measured gap today. What IS real
and worth fixing in base images: guests have no NTP bus (timedatectl
fails), so the async kick's `date -s` is the only clock correction —
build firecrackerctl base uVMs with systemd-timesyncd enabled for
self-disciplining clocks.

## P1: per-tap L3 isolation lock (sandbox-platform parity)

`warmnat.bpf.c` now enforces the Edge sandbox netns policy at the tap for
BOTH warm clones (NAT mode) and cold VMs (identity parameters, attached at
create and re-attached on daemon restart):

- **Source-address lock**: a guest may only speak as its own address.
  Spoofed sources are dropped (with `CFG_ENFORCE=1`).
- **Destination policy**: own /24 subnet allowed; the configured DNS resolver
  allowed; the warm NAT range (172.16/12), 192.168/16 outside the own
  subnet, 169.254/16 (metadata), 127/8 and multicast dropped; public
  internet passes via masquerade. Corporate 10/8 passes EXCEPT the
  host-infrastructure blocks in `FASTVM_L3_HOST_CIDRS` (default
  `10.242.0.0/16,10.226.12.0/24`: the fastvm host fast-net and the host
  mgmt vlan) — tenants reach query-prod (LiteLLM, loki, ...) but no host
  plane. Set `FASTVM_L3_HOST_CIDRS=none` (service-level drop-in or unit
  environment) to disable the 10/8 blocks entirely — corporate 10.x then
  flows unrestricted from guests. The setting is read per attach, so a
  daemon restart re-applies it to every live VM.
- **Host services unreachable**: packets aimed at the host-side tap peer
  pass ONLY into the ephemeral source-port window 32768-60999 — this is the
  stateless BPF equivalent of the netns "accept DNS + ct-established" input
  rule. sshd :22, the API :8090 etc. cannot be reached from any guest, while
  host-originated flows (socat proxy, engine SSH, exec) complete fine.
- **IPv6 is dropped** egressing the guest: the "enable IPv6, no route"
  guarantee, honestly.
- Opt-out: `FASTVM_L3_LOCK=0` (host-wide), resolver override
  `FASTVM_L3_DNS`. Default: on.
- Acceptance: `scripts/isolation-probe.sh` (run inside a guest) — 7/7 rows
  green on cold create, warm clones, and after daemon restarts.

Note: same-server VM-to-VM visibility is gone by design (their probe table
drops "another sandbox" too): every VM sits in its own L3 world.

## Sandbox-parity guest operations

- `fastvm pause/resume --vm X` — VMM-level pause with memory resident
  (`POST /api/v1/vms/{id}/pause|resume`), recorded in state.
- `fastvm exec --vm X --command 'uptime'` — non-interactive command over the
  engine SSH channel (`POST /api/v1/vms/{id}/exec`), JSON stdout/exit code.
- `fastvm expose --vm X --port 8080` — maps a guest TCP port to a host port
  in 61000-64999 via socat (`POST .../expose`, `unexpose`); mappings are
  recorded in VMState and reaped on destroy.
- `fastvm create ... --ttl MINUTES` — auto-destroy after N minutes; a
  handler-level sweeper (1/min) destroys expired VMs regardless of pause.
- **Entropy decorrelation**: every warm launch pushes 64 fresh host bytes
  into the guest CRNG pool in the hygiene kick, so clones of the same
  template no longer share identical RNG streams.

## Restored-guest first-connect: root-caused and fixed (gratuitous ARP)

Symptom (solved): the first host-initiated TCP into a freshly restored clone
sat dead ~25-40 s before the handshake committed; every later connection was
sub-millisecond. Root cause: the restored guest's neighbor entry for its
gateway still pointed at the SOURCE tap's frozen MAC; the fresh clone's tap
carried a new random MAC (templates without `source_tap_mac` metadata), so
the host silently discarded every guest frame addressed to the stale foreign
MAC until the guest's NUD re-validation forced a fresh ARP ~25-40 s in.
MAC-level tcpdump (`-e`) made it obvious: guest frames to
`c6:cc:0c:24:88:b9` while the live tap answered as `06:ae:b9:02:94:3c`.

Fix (warm nat layer): at attach, `warmAnnounceGateway` pushes gratuitous ARP
replies ("gateway is-at <current tap MAC>", unicast + broadcast, 8x/250 ms)
into the guest over an AF_PACKET socket, so the stale entry updates to the
fresh tap MAC immediately. Verified: 4/4 fresh clones accept TCP at +1 s,
exec at +2 s, hygiene kicks land instantly, isolation probes 8/8. New
templates also carry the source tap MAC in metadata, so their taps are pinned
to the frozen neighbor's MAC as well (belt and suspenders).

## Old caveat writeup (superseded): restored-guest first-connect latency

The first host-originated TCP connection into a freshly restored clone can
sit in SYN-retransmission ~5-30 s (longer under concurrent launches) before
the host stack commits the handshake; every attempt afterwards is
sub-millisecond. Bisect: NOT the L3 lock (reproduces with
`FASTVM_L3_LOCK=0`), not the NAT rewrites, not the static neighbor (verified
`ip neigh` PERMANENT + `ip route get` OK); the wire shows the guest SYN-ACKing
every SYN instantly while the host stack ignored them. Mitigation: the
hygiene kick retries for up to 90 s inside its async path, exec retries once;
steady-state is unaffected.
