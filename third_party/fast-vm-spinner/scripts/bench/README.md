# FastVM Backend Benchmarks

`compare-fastvm-backends.sh` drives the same `fastvm-api` contract against the
Proxmox and Firecracker backends and records lifecycle timings for:

- base VM creation
- workload staging/setup
- snapshot-to-template
- clone batch creation
- compile on cloned VMs
- converge/remove of losing clones
- winner destroy
- benchmark template cleanup

The compile workload uses `run-ffproxy-build.sh` inside each VM. Stage a local
checkout or tarball of `feature-flag-proxy-cpp`, plus a Bazel binary:

```bash
scripts/bench/compare-fastvm-backends.sh \
  --source-dir /path/to/feature-flag-proxy-cpp \
  --bazel-bin /tmp/bazel \
  --clone-count 2 \
  --cores 16 \
  --memory-mb 65536
```

Lifecycle-only smoke:

```bash
scripts/bench/compare-fastvm-backends.sh --skip-compile --clone-count 2
```

Results are written under `bench-results/<timestamp>/`:

- `events.jsonl` contains machine-readable timing events.
- `events.tsv` is easy to paste into a spreadsheet.
- `summary.md` is a Slack/PR-friendly summary.

By default the harness cleans up the VMs and snapshot templates it creates,
including on failed runs. Use `--keep-vms` or `--keep-template` only when
debugging a failed run.
