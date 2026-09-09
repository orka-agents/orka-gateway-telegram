<!-- markdownlint-disable MD013 -->

# Orka Telegram gateway adapter

`orka-gateway-telegram` implements Orka's `orka.gateway.v1` protocol outside Orka core. It maps private Telegram text messages into gateway events and stores update acknowledgements, delivery outcomes, and Telegram retry cooldowns in SQLite. Long replies are truncated at a Unicode character boundary with a visible suffix. Deleting the original Telegram message does not prevent a response from being sent.

The supported Kubernetes setup routes Telegram messages to a built-in Codex Agent on Orka's `orka.harness.v2` execution path. The gateway protocol remains `orka.gateway.v1`; it is separate from the Agent's harness protocol.

## Orka compatibility

This setup targets [Orka main at `597a8ab`](https://github.com/orka-agents/orka/tree/597a8abb08955a0be2c48931c4b24f52effb28c5). Use an existing Orka installation with:

- `--controller-mode=harness-v2`, `--gateway-enabled=true`, and the current CRDs;
- `--watch-namespace` set to one existing namespace labeled `orka.ai/controller-mode=harness-v2`;
- a configured, digest-pinned Codex ACP runtime image and a controller-managed provider authentication proxy;
- a model available through that provider proxy.

Orka requires its Gateway, GatewayBinding, and Agent resources in the controller's watched namespace. These manifests put the adapter in that same namespace and derive its ingress URL from the selected controller API and namespace. They do not create a namespace or change its mode claim. Follow Orka's [installation and mode guidance](https://github.com/orka-agents/orka/blob/597a8abb08955a0be2c48931c4b24f52effb28c5/website/docs/operations/harness-modes.md) when preparing or upgrading the controller.

The Codex Agent declares `contractVersion: orka.harness.v2`. Provider credentials belong to Orka's provider proxy, so the Agent has no `secretRef` and no temperature override. The earlier external harness v1 echo fixture is no longer part of this deployment.

## Build and test

```bash
make check
make build

# Check against a local checkout of the supported Orka source.
make test-orka-compatibility ORKA_DIR=/path/to/orka
```

Build a local image with `TAG=dev make image`. Publishing requires an explicit registry repository and a clean release tag:

```bash
export IMAGE=registry.example.com/example/orka-gateway-telegram
export TAG="$(git rev-parse HEAD)"

docker buildx inspect --bootstrap
make image-push
make inspect-image
```

The active buildx builder is used unless `BUILDER` names another configured builder. `PLATFORMS` defaults to `linux/amd64`; select the architectures used by the target cluster. Published builds include provenance and an SBOM. Enable a write-once tag policy in your registry and record the published digest. A tag's spelling cannot guarantee immutability.

## Configure the existing installation

The deployment needs Bash, `kubectl`, `jq`, `curl`, and `openssl`. The manifest tests also use Python 3 with PyYAML. Run the following examples in one Bash shell, replacing the example values with your installation's settings:

```bash
set -euo pipefail
export KUBE_CONTEXT=my-cluster-context
export NAMESPACE=my-orka-v2-namespace
export ORKA_API_URL="http://orka-api.${NAMESPACE}.svc.cluster.local:8080"
export AGENT_MODEL=your-provider-supported-codex-model
export IMAGE=registry.example.com/example/orka-gateway-telegram
export TAG="$(git rev-parse HEAD)"

# Optional. Omit this to use the cluster's default StorageClass.
# export STORAGE_CLASS=managed-csi

# Every command uses the selected context and namespace, including cluster-scoped reads.
k() {
  : "${KUBE_CONTEXT:?Set KUBE_CONTEXT}" "${NAMESPACE:?Set NAMESPACE}"
  kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" "$@"
}

namespace_mode="$(k get namespace "${NAMESPACE}" \
  -o 'jsonpath={.metadata.labels.orka\.ai/controller-mode}')"
[[ "${namespace_mode}" == harness-v2 ]] || {
  printf 'Choose an existing Orka harness-v2 watched namespace.\n' >&2
  exit 1
}
k get crd gateways.gateway.orka.ai gatewaybindings.gateway.orka.ai \
  gatewayclasses.gateway.orka.ai agents.core.orka.ai
```

`ORKA_API_URL` must name the API of the controller that watches `NAMESPACE`. It is a base URL without credentials or a path. Rendered deployments allow HTTP only for Kubernetes Service DNS ending in `.svc` or `.svc.cluster.local`; other hosts require HTTPS. The adapter uses `${ORKA_API_URL}/api/v1/gateways/${NAMESPACE}/telegram/events`. A controller watching another namespace rejects that request.

## Create the adapter Secrets

Keep mode-0600 source files outside the repository. For a first installation, save the BotFather token and generate three independent secrets. Reuse the existing files when upgrading:

```bash
export SECRET_DIR="${HOME}/.config/orka-gateway-telegram"
umask 077
mkdir -p "${SECRET_DIR}"
${EDITOR:-vi} "${SECRET_DIR}/telegram-bot-token"
for name in telegram-webhook-secret orka-gateway-inbound-token orka-gateway-outbound-token; do
  if [[ ! -e "${SECRET_DIR}/${name}" ]]; then
    openssl rand -hex 32 >"${SECRET_DIR}/${name}"
  fi
done
chmod 0600 "${SECRET_DIR}"/*

k create secret generic telegram-adapter-secrets \
  --from-file=telegram-bot-token="${SECRET_DIR}/telegram-bot-token" \
  --from-file=telegram-webhook-secret="${SECRET_DIR}/telegram-webhook-secret" \
  --dry-run=client --output=yaml | k apply -f -

for direction in inbound outbound; do
  k create secret generic "telegram-gateway-${direction}" \
    --from-file=token="${SECRET_DIR}/orka-gateway-${direction}-token" \
    --dry-run=client --output=yaml | k apply -f -
  k label secret "telegram-gateway-${direction}" \
    "gateway.orka.ai/${direction}-auth=true" \
    gateway.orka.ai/gateway-name=telegram --overwrite
  k annotate secret "telegram-gateway-${direction}" \
    gateway.orka.ai/gateway-name=telegram --overwrite
done
```

The generated Secret manifests go directly to Kubernetes. Do not print or commit them. The adapter mounts these four files through `TELEGRAM_BOT_TOKEN_FILE`, `TELEGRAM_WEBHOOK_SECRET_FILE`, `ORKA_GATEWAY_INBOUND_TOKEN_FILE`, and `ORKA_GATEWAY_OUTBOUND_TOKEN_FILE`. A secret value and its corresponding `*_FILE` variable are mutually exclusive.

## Deploy the adapter

The adapter uses one replica, a `Recreate` rollout, and one ReadWriteOnce PVC. It runs as a non-root user with a read-only root filesystem, no ServiceAccount token, and read-only Secret mounts. Keep the replica count at one while SQLite owns durable state. The PVC uses the cluster's default StorageClass unless `STORAGE_CLASS` is set; keep that choice unchanged on subsequent deployments.

For an existing bot, preserve its SQLite history before deploying. Moving from the old adapter namespace to Orka's watched namespace creates a different PVC, even when its name is unchanged. Stop incoming traffic and the old adapter, take a consistent SQLite backup, and restore it into a pre-created `orka-gateway-telegram-data` PVC in the target namespace before starting the new adapter. Preserve the database and any required WAL state, file ownership for UID/GID 65532, and the existing bot and gateway credentials. Retain the original PVC for recovery. Starting with an empty database loses duplicate-delivery protection; never run the old and new adapters for the same bot at the same time.

Render the complete non-secret manifests for inspection, then deploy:

```bash
export MANIFEST_DIR="$(mktemp -d)"
make render-manifests >"${MANIFEST_DIR}/adapter.yaml"
make deploy
```

Rendering is local and does not contact Kubernetes. Deployment verifies the existing namespace's mode before applying resources. The base exposes a ClusterIP Service on port 8080. Its `/healthz` and `/readyz` probes do not need the bearer token required by the gateway protocol routes.

For a stable endpoint, configure an HTTPS ingress or tunnel forwarding to `http://orka-gateway-telegram:8080` in this namespace, then set `ADAPTER_URL` to its public HTTPS base URL. For temporary validation, the optional tunnel below supplies that URL.

## Optional temporary Quick Tunnel

The existing pinned `cloudflared` image can expose the adapter without a local port-forward:

```bash
./scripts/render-manifests.sh tunnel >"${MANIFEST_DIR}/tunnel.yaml"
k apply -f "${MANIFEST_DIR}/tunnel.yaml"
k rollout status deployment/telegram-quick-tunnel --timeout=5m
k logs deployment/telegram-quick-tunnel --tail=100
```

Copy the generated public URL, without a path or trailing slash:

```bash
export ADAPTER_URL=https://replace-with-generated-host.trycloudflare.com
```

A Quick Tunnel hostname changes when its Pod is recreated. Use a stable HTTPS ingress or named tunnel for persistent operation. See the [Cloudflare Quick Tunnel documentation](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/) for its limits.

## Connect the Codex Agent

Set the exact numeric bot, private chat, and sender IDs. The bot ID is the non-secret numeric prefix before the colon in a valid BotFather token. In a private conversation, the chat and sender IDs normally match:

```bash
export TELEGRAM_ACCOUNT_ID="$(cut -d: -f1 <"${SECRET_DIR}/telegram-bot-token")"
export TELEGRAM_CHAT_ID=123456789
export TELEGRAM_SENDER_ID=123456789
: "${ADAPTER_URL:?Set the public HTTPS adapter base URL}"

# GatewayClass is shared and cluster-scoped. Reuse this class if already installed.
k apply -k deploy/cluster
k annotate secret telegram-gateway-outbound \
  gateway.orka.ai/adapter-endpoint="${ADAPTER_URL%/}" --overwrite

./scripts/render-manifests.sh routing >"${MANIFEST_DIR}/routing.yaml"
k apply -f "${MANIFEST_DIR}/routing.yaml"

config_patch="$(jq -cn --arg url "${ADAPTER_URL%/}/telegram/webhook" \
  '{data:{TELEGRAM_WEBHOOK_URL:$url}}')"
k patch configmap orka-gateway-telegram --type=merge --patch "${config_patch}"
unset config_patch
k rollout restart deployment/orka-gateway-telegram
k rollout status deployment/orka-gateway-telegram --timeout=5m
```

The routing manifests create `Agent/telegram-ai`, `Gateway/telegram`, and `GatewayBinding/telegram-ai`. The binding permits only the selected bot, chat, and sender, and queues messages while a turn is active. The class is `GatewayClass/telegram-chat-live-c65ebad8`; its existing name is retained for installations that already share it.

Keep `ADAPTER_URL` exported for later `make deploy` commands so rendering preserves the webhook URL. After a Quick Tunnel URL changes, update the existing route, Secret endpoint annotation, and webhook together:

```bash
ADAPTER_URL="$(./scripts/reconcile-quick-tunnel.sh)"
export ADAPTER_URL
```

The reconciler requires `KUBE_CONTEXT`, `NAMESPACE`, an existing harness-v2 mode claim, and the routing objects from the previous step. It prints only the new public URL.

## Verify the route

```bash
ORKA_GATEWAY_OUTBOUND_TOKEN_FILE="${SECRET_DIR}/orka-gateway-outbound-token" \
  make live-validate
```

Validation checks the adapter Deployment, PVC, Service, and Secret object presence; authenticated protocol health and capabilities through a temporary local port-forward; and current-generation GatewayClass, Agent, Gateway, and GatewayBinding readiness. It checks the Codex v2 Agent configuration and that the ingress URL names the selected namespace. It does not read Kubernetes Secret data or send Telegram messages.

Then send a private message from the configured Telegram account. Verify that it produces a reply and advances the binding's activity timestamps:

```bash
k get gatewaybinding/telegram-ai \
  -o 'jsonpath={.status.lastInboundActivity}{" -> "}{.status.lastOutboundActivity}{"\n"}'
k get tasks,sessions
```

## Configuration and upgrades

For local execution, copy `.env.example` and provide the full Orka ingress URL plus the four secret file paths. The file is an example; the adapter reads its configuration from the process environment.

| Variable | Deployment value | Purpose |
| --- | --- | --- |
| `LISTEN_ADDRESS` | `:8080` | Adapter listener |
| `DATABASE_PATH` | `/data/telegram-adapter.db` | Durable SQLite database |
| `TELEGRAM_API_BASE_URL` | `https://api.telegram.org` | Telegram API, overridable for trusted tests |
| `TELEGRAM_WEBHOOK_URL` | Derived from `ADAPTER_URL`, or empty during bootstrap | Full Telegram callback URL |
| `TELEGRAM_DROP_PENDING_UPDATES` | `false` | Webhook registration behavior |
| `ORKA_GATEWAY_INGRESS_URL` | Derived from `ORKA_API_URL` and `NAMESPACE` | Selected Orka Gateway events endpoint |
| `TELEGRAM_CONFORMANCE_CHAT_ID` | `0` | Chat for explicitly requested delivery conformance checks |
| `REQUEST_TIMEOUT` | `15s` | Outbound request timeout |
| `SHUTDOWN_TIMEOUT` | `20s` | Graceful shutdown deadline |

Schema version 2 adds stable idempotency indexes; version 3 adds durable Telegram cooldown timestamps. Current Orka uses equal `deliveryId` and `idempotencyId` values, which the upgrade maps to existing delivery records. Older callers using distinct values must retry the original `deliveryId` once after upgrading before rotating that ID, because schema version 1 did not store their distinct idempotency ID. Delivery records remain durable without time-based pruning.

Back up SQLite consistently with its WAL before upgrading. The schema migrations are forward-only. To run an older adapter after a migration, restore its matching pre-upgrade database snapshot first. Keep the PVC during maintenance and cleanup.

Rotate Telegram and Orka tokens independently, update their Secret files, and restart the adapter so it reloads them. When changing a public endpoint, update the webhook, Gateway endpoint, and outbound Secret endpoint annotation together. Provider credentials continue to be managed by Orka's provider proxy.

## Cleanup

Remove the bot's webhook before retiring its public endpoint. Keep the bot token out of command arguments by using a protected temporary curl config:

```bash
delete_webhook_config="$(mktemp)"
chmod 0600 "${delete_webhook_config}"
bot_token="$(tr -d '\r\n' <"${SECRET_DIR}/telegram-bot-token")"
printf 'url = "https://api.telegram.org/bot%s/deleteWebhook"\nrequest = "POST"\nsilent\nshow-error\nfail\n' \
  "${bot_token}" >"${delete_webhook_config}"
unset bot_token
curl --config "${delete_webhook_config}"
rm -f "${delete_webhook_config}"

k delete gatewaybinding/telegram-ai gateway/telegram agent/telegram-ai --ignore-not-found
k delete deployment/telegram-quick-tunnel --ignore-not-found
k delete deployment/orka-gateway-telegram service/orka-gateway-telegram \
  configmap/orka-gateway-telegram serviceaccount/orka-gateway-telegram --ignore-not-found
k delete secret/telegram-adapter-secrets secret/telegram-gateway-inbound \
  secret/telegram-gateway-outbound --ignore-not-found
```

This retains the adapter PVC, the shared GatewayClass, and the Orka namespace. Remove shared resources or persistent data only as a separate, deliberate operation.

## Manifest layout

| Path | Purpose |
| --- | --- |
| `deploy/` | Adapter Deployment, Service, identity, configuration, and PVC |
| `deploy/routing/` | One Codex v2 Agent, Gateway, and private-chat binding |
| `deploy/cluster/` | Shared, cluster-scoped GatewayClass |
| `deploy/tunnel/` | Optional temporary Quick Tunnel |
| `scripts/render-manifests.sh` | Local namespace, image, endpoint, and model rendering |

The renderer's default `adapter` mode requires `NAMESPACE`, `ORKA_API_URL`, `IMAGE`, and `TAG`, with optional `ADAPTER_URL` and `STORAGE_CLASS`. `routing` requires `NAMESPACE`, `ADAPTER_URL`, `AGENT_MODEL`, and the three Telegram identity variables. `tunnel` requires only `NAMESPACE`. `KUBECTL` selects the kubectl binary. Apply rendered files; the raw bases contain placeholders.
