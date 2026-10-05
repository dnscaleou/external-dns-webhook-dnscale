# DNScale ExternalDNS webhook

Connect [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) to DNScale
to manage Kubernetes Service and Ingress DNS in existing zones. The provider
runs as an HTTP sidecar alongside the upstream controller.

## Supported behavior

- A and AAAA records with multiple targets, CNAME records, and TXT ownership.
- Target and TTL changes, `create-only`, `upsert-only`, `sync`, and dry-run.
- Explicit domain and zone ID filters, including exclusions and nested zones.
- Paginated discovery, selected-value deletion, and recovery after partial writes.
- Linux AMD64 and ARM64 images; non-root runtime and a read-only filesystem.

The compatibility baseline is **ExternalDNS v0.23.0**, official chart **1.23.0**,
and Kubernetes **1.35.0**. Protocol, race, fault-replay, and actual-controller
tests are included. The adapter uses Go 1.25 and an immutable DNScale SDK
revision; it does not import the controller's Kubernetes dependency tree.

Wildcards, arbitrary TXT data, apex CNAME flattening, automatic zone creation,
routing policies, and encrypted or customized TXT registry formats are not
supported. Existing unsupported record types remain untouched. Disabled records
in the managed scope cause an explicit conflict.

## Install

Use a dedicated delegated zone, such as `k8s.example.org`. The API token needs
`zones:read`, `records:read`, and `records:write`, restricted to that zone.
DNScale DNS-name scopes match exact names, not subtrees; if using them, include
both application names and their ownership TXT names. Zone creation permission
is unnecessary.

1. Create the application and controller namespaces, then mount the credential
   from a local file. Do not put the token in Helm values or command arguments.

   ```sh
   kubectl create namespace applications
   kubectl create namespace external-dns
   kubectl -n external-dns create secret generic dnscale-api-token \
     --from-file=token=/secure/path/dnscale-api-token
   ```

2. Download [deploy/values.yaml](deploy/values.yaml) from the `v1.0.0` release.
   Replace the example domain in **both** `domainFilters` and
   `DNSCALE_DOMAIN_FILTER`, the zone UUID in `DNSCALE_ZONE_ID_FILTER`, and the
   stable owner ID in **both** `txtOwnerId` and `DNSCALE_TXT_OWNER_ID`.
   Set `sourceNamespace` and `labelFilter` for the applications to manage.
   Keep `txtPrefix: "edns-%{record_type}."` unchanged.

3. Install the pinned official chart. The supplied values start in dry-run.

   ```sh
   helm upgrade --install external-dns external-dns \
     --repo https://kubernetes-sigs.github.io/external-dns \
     --version 1.23.0 --namespace external-dns \
     --values values.yaml --wait
   kubectl -n external-dns logs deployment/external-dns -c external-dns
   kubectl -n external-dns logs deployment/external-dns -c webhook
   ```

4. Review the selected resources and targets. Set the sidecar's
   `DNSCALE_DRY_RUN` environment value to `"false"` and
   `extraArgs.dry-run: false`, then repeat the Helm command to enable
   `upsert-only` writes. Enable `policy: sync`
   only when deletion of DNS for removed Kubernetes resources is desired.

Use the [Service](examples/service.yaml) or [Ingress](examples/ingress.yaml)
example, changing its namespace and hostname to match your configuration.
The resources must carry the configured label. The supplied namespace-scoped
configuration includes LoadBalancer and ExternalName Services; it excludes
NodePort and ClusterIP sources that require additional informer permissions.
A load balancer or ingress
controller must populate their status with an IP address or hostname.
Annotations use the current `external-dns.kubernetes.io/` prefix.

The sidecar image is
`ghcr.io/dnscaleou/external-dns-webhook-dnscale:v1.0.0`. Pin an image digest for
controlled upgrades. The chart is provided by the ExternalDNS project; there
is no separate DNScale chart or cert-manager APIService to install.

## Ownership and safe operation

**Dry-run is enforced by `DNSCALE_DRY_RUN` in the sidecar, and defaults to true.**
ExternalDNS v0.23.0 does not forward its own `--dry-run` flag through the webhook
protocol. That controller flag alone cannot prevent webhook writes. Set the
sidecar variable to `"true"` whenever validating configuration without writing
DNS; its logs report validated batches without exposing record contents.

Run **one active controller per ownership scope**, using one replica and the
included `Recreate` deployment strategy. Do not have Terraform, DNSControl,
another ExternalDNS instance, or manual automation write the same name/type
record sets. The API does not expose a distributed compare-and-swap operation;
a provider preflight read cannot protect against another writer racing it.

Ownership belongs to a complete record set, not to individual IP addresses.
Manually added values in an owned set can be treated as drift by ExternalDNS.
The provider refuses to adopt existing unowned records, overwrite foreign
ownership markers, or operate beyond its configured filters. TXT ownership
coordinates controllers; API token scopes provide authorization.

Ownership TXT is established before data creation and retained until data
deletion completes. Failed batches return errors and are re-read on retry.
This ordering supports recovery after a response is lost or the process exits;
it does not make a multi-record change atomic. A failed creation may leave an
orphaned ownership TXT. Retrying the same desired resource recovers it; if the
resource was withdrawn, pause reconciliation and remove only the orphan after
checking that its data is absent.

Keep owner IDs and the TXT prefix stable across upgrades. To adopt existing
records, first stop their previous writer, export the records, and explicitly
establish a matching ownership marker after reviewing the complete RRset.
Changing an owner ID is a deliberate migration, not a routine upgrade.

CNAME creation replaces an existing CNAME in the API. Exclusive writers are
therefore required. The provider uses selected-record updates for target
changes and selected-record deletes, never whole-RRset delete fallbacks.
Self-hosted APIs must preserve a replacement when an old CNAME ID is deleted
before enabling CNAME `sync`.

Give cert-manager its own `_acme-challenge` names; this provider writes TXT only
for the fixed ExternalDNS ownership format. Unrelated ACME and manual records
remain outside its writes. Exclude delegated subzones the controller must not
manage; a known, more specific zone outside the zone allowlist is never replaced
by a parent-zone fallback.

## Configuration

| Environment variable | Meaning / default |
| --- | --- |
| `DNSCALE_API_TOKEN_FILE` | Required path to the mounted token |
| `DNSCALE_DOMAIN_FILTER` | Required comma-separated DNS suffix allowlist |
| `DNSCALE_ZONE_ID_FILTER` | Required comma-separated zone UUID allowlist |
| `DNSCALE_TXT_OWNER_ID` | Required stable controller owner ID |
| `DNSCALE_DRY_RUN` | `true`; explicitly set `false` to allow writes |
| `DNSCALE_EXCLUDE_DOMAINS` | Optional comma-separated suffix exclusions |
| `DNSCALE_DEFAULT_TTL` | `300`; accepted range is 300–86400 seconds |
| `DNSCALE_API_URL` | `https://api.dnscale.eu/v1` |
| `DNSCALE_API_TIMEOUT` | `15s` per SDK operation |
| `DNSCALE_REQUEST_TIMEOUT` | `90s` per webhook request |
| `DNSCALE_LISTEN` | `127.0.0.1:8888`; loopback binding required |
| `DNSCALE_HEALTH_LISTEN` | `0.0.0.0:8080` |

The token is loaded at startup. Update the Secret and restart the deployment
to rotate it. Redirects never receive credentials. HTTPS is required;
`DNSCALE_ALLOW_HTTP=true` exists only for local API fixtures.

`GET /healthz`, `GET /readyz`, and Prometheus `GET /metrics` use the separate
health listener. Readiness means valid configuration and initialized handlers;
API outages are reported as reconciliation errors, not restart loops. Metrics
cover requests, total duration, failures, throttling, read operations, and
successful writes. Read-operation counters cover a complete SDK list, which
may span several HTTP pages. Logs omit API response bodies and record targets;
upstream controller logs can contain application DNS names.

There is no inventory cache. A reconciliation lists every accessible zone page
and every selected zone's record pages. A write batch performs another complete
preflight read, then one request per necessary mutation. With page size 100,
budget approximately `ceil(zones/100) + sum(ceil(records_in_zone/100))` reads per
inventory, counting an empty zone as one page. Start with the supplied two-minute
interval and measure API quota use before increasing frequency. Rate limiting
blocks subsequent SDK operations for the retry delay; request deadlines bound
waiting, and upstream retries re-read state. SDK writes are not blindly retried.

To stop reconciliation, scale the deployment to zero. Preserve DNS and TXT
ownership while investigating. A Helm rollback does not reverse DNS writes.
Restore reviewed records from an export before resuming if recovery is needed.

## Development and tests

```sh
GOCACHE="$PWD/.gocache" go test -race ./...
GOCACHE="$PWD/.gocache" go vet ./...
docker build -t dnscale-external-dns:e2e .
python3 scripts/e2e.py
```

The integration script requires Docker, kind, kubectl, and Helm. It creates a
temporary kind cluster, installs the official chart, and runs the pinned
controller against an API fixture with forced pagination. It covers Services,
Ingresses, all supported data types, policies, target/TTL updates, restart,
no-op reconciliation, and preservation of unrelated records.

For an opt-in live run, additionally provide `DNSCALE_E2E_DOMAIN`,
`DNSCALE_E2E_ZONE_ID`, and `DNSCALE_E2E_TOKEN_FILE`, then run
`python3 scripts/e2e.py --live`. The domain must already have public NS
delegation. The script creates a random namespace below that domain, verifies
public DNS, and cleans up only its run's records. It requires `dig` and real
write permission. DNS checks allow 660 seconds for the tested TTLs to expire;
override with `--dns-timeout` if needed. Live tests never run in ordinary
pull-request CI. Keep all
tokens, kubeconfigs, customer names, and private test reports out of Git.

## License

Apache-2.0. The wire contract follows the
[upstream webhook protocol](https://github.com/kubernetes-sigs/external-dns/blob/v0.23.0/provider/webhook/api/httpapi.go).
