# Spec: FastVM Template Lifecycle And Quotas

## Objective

Allow users to create, reuse, and remove FastVM templates without asking Infra
BM for every template, while preventing insecure images, unbounded storage
growth, and unclear ownership.

Success means a non-Infra BM user can:

- create a VM from an approved security baseline
- customize it
- snapshot it into a reusable template
- list their templates
- create new VMs from those templates
- delete templates they no longer need

## Current Assumptions

- The existing `fastvm` client remains the only user CLI.
- Custom templates stay numeric so the existing `-t/--template` flag keeps
  working for both Proxmox and Firecracker.
- Template `9013` is the current approved security baseline.
- Runtime secrets and enrolled agent identity are not baked into templates.
- The first implementation is owner-private templates only; shared/promoted
  templates require an explicit operator workflow.

## Commands

Build and test commands for this repo:

```bash
go test ./internal/fastvm
go test ./...
make build
```

Expected user commands after implementation:

```bash
fastvm create -u alice -c 1 -t 9013 --cores 16 -m 65536
fastvm snapshot --vm 60000 --template-name alice-rust-builder
fastvm templates list -u alice
fastvm create -u alice -c 1 -t 60000
fastvm templates delete 60000
```

## Lifecycle

| State | Meaning | Allowed transitions |
| --- | --- | --- |
| `active-vm` | Running VM owned by a user | snapshot, destroy |
| `template` | Reusable owner-private template | clone, rename, delete, promote request |
| `promoted` | Operator-approved shared template | clone, deprecate |
| `deprecated` | Existing clones still work, new creates blocked by default | delete |
| `deleted` | Backend artifact and registry entry removed | none |

Snapshots must be rejected unless the source VM was created from an approved
security template or another registered secure template.

## Metadata

Template registry entries should include:

- template ID
- user-visible name
- owner user
- backend: `proxmox` or `firecracker`
- visibility: `private`, `shared`, or `deprecated`
- source VMID
- source template ID/name
- security baseline marker
- created timestamp
- last-used timestamp
- optional expiration timestamp
- size bytes when the backend can report it

Firecracker also persists the security marker into
`/opt/fastvm-microvm/templates/<id>.env` so the backend can recover from a
missing or stale registry file. Proxmox stores template metadata in the FastVM
state registry because Proxmox template VMIDs do not carry enough FastVM-owned
metadata.

## API Shape

Keep the existing snapshot endpoint:

```http
POST /api/v1/vms/{vmid}/snapshot
```

Add template lifecycle endpoints:

```http
GET    /api/v1/templates?user=alice
GET    /api/v1/templates/{template}
PATCH  /api/v1/templates/{template}
DELETE /api/v1/templates/{template}
```

The delete endpoint should remove both the registry entry and backend artifact:

- Proxmox: destroy the template VMID with purge.
- Firecracker: remove `templates/<id>.ext4` and `templates/<id>.env`.

## CLI Shape

Keep `fastvm create -t TEMPLATE` compatible.

Add a small template subcommand group:

```bash
fastvm templates list [-u USER]
fastvm templates get TEMPLATE
fastvm templates rename TEMPLATE NAME
fastvm templates delete TEMPLATE
```

`snapshot` should accept an optional display name:

```bash
fastvm snapshot --vm 60000 --template-name alice-rust-builder
```

## Quotas And TTL

Recommended first limits:

- max private templates per user: 5
- max private template age: 30 days by default
- max templates per backend host: configurable, default 200
- max total Firecracker template bytes: configurable, warn at 80 percent and
  reject new snapshots at 90 percent

Quota failures should happen before destructive snapshot work starts. Template
delete should always be allowed for the owner even when quotas are exceeded.

Default TTL behavior:

- Private templates get `expires_at = created_at + 30d`.
- Creating a VM from a template refreshes `last_used_at`, not `expires_at`.
- Expired templates are hidden from default list output and blocked for create.
- Garbage collection deletes expired private templates after a 7-day grace
  period.

## Cleanup Behavior

Add a periodic backend cleanup loop:

- load `templates.json`
- mark expired private templates as deprecated
- delete deprecated templates after the grace period
- log every deletion with owner, backend, template ID, size, and reason
- never delete promoted templates automatically

If backend artifact deletion fails, keep the registry entry and mark it with a
cleanup error so it can be retried.

## Testing Strategy

Unit tests:

- quota checks before snapshot
- owner filtering in template list
- registry persistence and reload
- Firecracker artifact path safety
- delete behavior with mocked backend operations

Integration smoke:

- create from `9013`
- snapshot with name
- list template
- clone from template
- delete template
- verify clone still runs and deleted template cannot be used

Run the integration smoke once against Proxmox and once against Firecracker
before enabling the delete endpoint for users.

## Boundaries

Always:

- require a security baseline before snapshot
- keep template ownership explicit
- make delete idempotent
- run backend package tests before commit

Ask first:

- changing the default quota values
- promoting templates to shared visibility
- deleting existing templates that are not in `templates.json`

Never:

- bake Vault tokens, enrolled agent state, SSH host keys, or machine-id into a
  template
- allow path-like Firecracker template names
- auto-delete promoted templates

## Recommended First Implementation Slice

1. Extend `TemplateState` with name, visibility, expiry, and last-used fields.
2. Add backend interface methods for list/get/delete templates.
3. Add API routes for `GET /templates` and `DELETE /templates/{template}`.
4. Add `fastvm templates list/delete`.
5. Enforce max private templates per user before snapshot.
6. Add a Firecracker delete implementation and a Proxmox template destroy
   implementation.

This keeps the next slice small and useful: users can see and remove their own
templates before we add sharing, promotion, or automatic garbage collection.

## Open Tradeoffs

- Whether a template display name must be globally unique or only unique per
  owner.
- Whether `expires_at` should refresh on use for heavily used private templates.
- Whether promoted templates should stay numeric or get a separate named alias.
- Whether quota storage accounting should block Proxmox snapshots before a
  reliable backend byte-size API exists.
