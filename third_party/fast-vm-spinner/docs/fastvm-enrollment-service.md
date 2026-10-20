# FastVM Enrollment Service

FastVM templates should contain packages and static baseline config only.
Secrets, host identity, and security-agent enrollment should happen after a VM
is cloned. The recommended implementation is a small Kubernetes service with
Vault access that enrolls each VM through the same SSH proxy FastVM returns to
users.

## Why A Service

Do not give Vault credentials to the VMs. Users have root in FastVM guests, and
custom templates can be snapshotted and cloned. Any Vault token, Lacework token,
Falcon provisioning secret, or SSH signing credential placed in the guest should
be treated as leaked.

Instead, keep Vault access in Kubernetes:

1. `fastvm-api` creates the VM and waits for SSH.
2. `fastvm-api` calls the enrollment service with VM metadata.
3. The enrollment service reads required static secrets from Kubernetes Secrets
   synced from Vault.
4. The enrollment service connects to the VM through the FastVM SSH proxy and
   writes only the rendered guest config.
5. The VM never receives reusable Vault credentials.

This matches the bare-metal application workflow documented in
`team-telnyx/infra-bare-metal-application-workflow`: use a Kubernetes
`ServiceAccount`, `ServiceId`, `VaultAuth`, and `VaultStaticSecret`, then deploy
through `deploy-infra-bare-metal-main` with cluster-specific overlays.

## Service Shape

Implemented service name: `fastvm-enroller`.

Source artifacts in this repo:

- `cmd/fastvm-enroller`: HTTP service binary.
- `internal/enroller`: enrollment API, SSH runner, and HTTP client.
- `Dockerfile.enroller`: OCI image for Kubernetes.
- `deployments/fastvm-enroller/k8s`: Kustomize base plus an AT1 overlay.

Build:

```bash
make build-enroller
make build-enroller-image
```

Enrollment API:

```http
POST /v1/enroll
Authorization: Bearer <fastvm-api-to-enroller-token>
Content-Type: application/json
```

```json
{
  "vmid": "60000",
  "name": "fastvm-alice-1",
  "user": "alice",
  "backend": "firecracker",
  "template": "plain-ubuntu",
  "node": "atl1-pc1-027",
  "ssh_host": "atl1-pc1-027.hosts.telnyx.io",
  "ssh_port": 60000,
  "ssh_user": "root",
  "features": ["host-identity", "authorized-keys", "lacework", "crowdstrike", "auditd", "fluent-bit"]
}
```

The service runs enrollment synchronously and returns `200 OK` once the VM has
been updated. `fastvm-api` keeps VM creation fast by calling this endpoint in a
background goroutine when `FASTVM_ENROLLER_MODE=async`.

Status API:

```http
GET /v1/enrollments/{vmid}
Authorization: Bearer <fastvm-api-to-enroller-token>
```

Status is process-local and intended for operational debugging. It is not a
durable audit log.

## Vault Access

Use read-only Vault policies for static secrets. Keep each path explicit:

- FastVM bootstrap/enroller SSH key, for connecting through the host SSH proxy.
- Authorized public keys to place in the VM SSH user account.
- Lacework access token.
- CrowdStrike customer ID/provisioning values.
- Shared API token used by `fastvm-api` to call `fastvm-enroller`.

The Kustomize base syncs the FastVM-owned bootstrap material from
`infra-bare-metal-squad/fastvm-enroller`, rendered into a Kubernetes Secret
named `fastvm-enroller`. Expected keys:

| Key | Purpose |
| --- | --- |
| `id_rsa` | Private key used by the enroller to connect through the FastVM SSH proxy |
| `token` | Bearer token accepted by `fastvm-enroller` and sent by `fastvm-api` |
| `authorized_keys` | Public keys appended idempotently to the guest SSH user's `authorized_keys` |

Security tooling credentials are synced from the same Vault paths used by the
existing VM roles:

| Role | Vault API path | Synced key | Enroller file |
| --- | --- | --- | --- |
| `infra-roles-lacework-agent` | `infraops-squad/data/infra-roles-lacework-agent` | `agent_access_token` | `/run/secrets/fastvm-enroller-lacework/agent_access_token` |
| `infra-roles-crowdstrike-falcon` | `infraops-squad/data/infra-roles-crowdstrike-falcon` | `crowdstrike_customer_id` | `/run/secrets/fastvm-enroller-crowdstrike/crowdstrike_customer_id` |

The CrowdStrike role also stores `artifactory_api_key` at the same path for
package download, but the enroller only needs the customer ID because guest
images already contain the agent package.

Dynamic host certificate signing is different from static secret sync. If we
need SSH host certificates for each clone, the service should authenticate to
Vault with Kubernetes auth and call the SSH signing endpoint per VM. Do not try
to pre-render per-VM host certs into a template.

Example policy shape:

```yaml
apiVersion: infra-core.telnyx.com/v1alpha1
kind: ServiceId
metadata:
  name: fastvm-enroller
  namespace: fastvm
spec:
  k8s_sa_name: fastvm-enroller
  k8s_sa_namespace: fastvm
  vault_policy_name: fastvm-enroller
  vault_policies:
    - path: "infra-bare-metal-squad/data/fastvm-enroller"
      capabilities: ["read", "list"]
    - path: "infra-bare-metal-squad/data/fastvm-enroller/*"
      capabilities: ["read", "list"]
    - path: "infraops-squad/data/infra-roles-lacework-agent"
      capabilities: ["read", "list"]
    - path: "infraops-squad/data/infra-roles-crowdstrike-falcon"
      capabilities: ["read", "list"]
    # Needed only if the service signs SSH host certs directly.
    - path: "ssh-host/sign/hostrole"
      capabilities: ["update"]
  vault_k8s_auth_role_name: fastvm-enroller
  k8s_sa_rules: []
```

The exact Vault paths and cross-squad policy ownership need Security/InfraOps
approval before production rollout.

## Guest Enrollment Steps

The service currently executes these steps over SSH:

1. Regenerate clone identity:
   - reset `/etc/machine-id`
   - generate missing SSH host keys
2. Configure host access:
   - append Vault-synced authorized public keys to the guest SSH user
3. Configure security agents:
   - write Lacework config from Vault-backed token
   - run Falcon `falconctl` with CID/tags
   - start/enable `lacework` and `falcon-sensor`
4. Configure logging/audit:
   - start `auditd` and `fluent-bit` when present
5. Mark completion:
   - write `/var/lib/fastvm/enrollment.json`
   - include enrollment version, VMID, timestamp, and successful feature list

The service should be idempotent. A retry must not append duplicate config or
re-enroll a running agent with conflicting identity.

## FastVM API Integration

Configure `fastvm-api` with optional environment variables:

```text
FASTVM_ENROLLER_URL=http://fastvm-enroller.query.prod.telnyx.io:8080
FASTVM_ENROLLER_TOKEN_FILE=/run/secrets/fastvm-enroller/token
FASTVM_ENROLLER_MODE=async
FASTVM_ENROLLER_FEATURES=host-identity,authorized-keys,lacework,crowdstrike,auditd,fluent-bit
```

The Kubernetes deployment registers `fastvm-enroller` with Consul Connect, so
Firecracker API hosts should use the Consul-backed
`fastvm-enroller.query.prod.telnyx.io` name rather than cluster-local
Kubernetes DNS. Inside the Kubernetes cluster the service also resolves as
`fastvm-enroller.service.consul`.

Suggested modes:

- `disabled`: do not enroll VMs.
- `async`: create VM, start enrollment in a background goroutine, and return VM
  details immediately with an `enrollment` response summary.
- `required`: create VM, run enrollment, return only after success.

For the first PoC, use `async` so FastVM keeps its fast create path and we can
measure actual enrollment overhead separately.

`fastvm-api` sends the VMID, user, backend, template, node, SSH proxy host,
SSH proxy port, SSH user, and configured features to the service.

## Template Interaction

Templates may include:

- installed Lacework/Falcon packages, but stopped and unconfigured
- auditd package and static rules
- fluent-bit package/config, disabled until clone metadata exists
- host-access users, sudoers, SSH CA trust

Templates must not include:

- Lacework token/config
- Falcon enrolled AID/state
- SSH host private keys or host certificates
- Vault credentials
- machine-id
- cloud-init instance state

Before `fastvm snapshot`, a template cleanup hook should remove agent state and
identity files. After clone, the enrollment service recreates the per-VM state.

### Baking Falcon Into Firecracker Templates

The Firecracker `plain-ubuntu` template should include the Falcon package but
must not include the CrowdStrike CID, AID, falconstore, Registry files, or any
running sensor state. The enroller sets the CID/tags after each clone is created.

Use the host-side helper to update an existing template from a downloaded
`falcon-sensor` deb:

```bash
sudo FALCON_SENSOR_DEB=/tmp/falcon-sensor_7.07.0-16206_amd64.deb \
  FALCON_SENSOR_SHA256=9c06b33702d38f28b19027d8d6072f1afb5045886e80ef85c72eeeefd035533d \
  scripts/firecracker/fc-install-falcon-template.sh
```

The helper works on a reflink/sparse copy, installs Falcon dependencies, runs
`dpkg -i`, removes clone-unsafe identity and Falcon state, verifies
`/opt/CrowdStrike/falconctl`, then atomically replaces the template while
leaving a timestamped `*.pre-falcon.*` backup.

### Baking The Firecracker Security Template

Template `9013` is the Firecracker security baseline. It is created from
`plain-ubuntu` and contains the clone-safe package/config baseline for:

- CrowdStrike Falcon package, with CID/tags applied by the enroller
- Lacework package, with `/etc/lacework/config.json` written by the enroller
- `auditd` with Telnyx baseline rules
- Fluent Bit forwarding journald and `/var/log/audit/audit.log`

Build or replace it on each Firecracker host with:

```bash
sudo TARGET_TEMPLATE=9013 SOURCE_TEMPLATE=plain-ubuntu \
  scripts/firecracker/fc-create-security-template.sh
```

Use `FORCE=1` to replace an existing `9013` image. The helper writes a
timestamped backup before replacement, clears `/etc/machine-id`, and verifies
the expected packages, systemd units, audit rules, and Fluent Bit audit tail.
