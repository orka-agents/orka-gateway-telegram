<!-- markdownlint-disable MD013 -->

# Orka Telegram gateway adapter

`orka-gateway-telegram` is an out-of-tree Telegram adapter for Orka's exact-versioned `orka.gateway.v1` protocol. It keeps Telegram payloads and credentials at the adapter boundary, maps supported private text messages into normalized Orka gateway events, and durably deduplicates Telegram updates and outbound deliveries in SQLite.

Outbound responses that exceed Telegram's `sendMessage` text limit are safely truncated on a Unicode character boundary with a visible suffix. Replies also opt into Telegram's send-without-reply fallback so deleting the originating message while an AI turn is running does not discard the final response.

## Deployment shape

The checked-in Kubernetes base is intentionally conservative:

- one adapter replica;
- `Recreate` deployment strategy so two SQLite writers are never started during an update;
- one AKS `managed-csi` `ReadWriteOnce` PVC mounted at `/data`;
- a `ClusterIP` Service only—there is no public load balancer;
- a non-root distroless image, read-only root filesystem, dropped Linux capabilities, `RuntimeDefault` seccomp, no ServiceAccount token, and restricted Pod Security labels;
- credentials mounted as read-only Secret files rather than ordinary environment values;
- HTTP startup/readiness/liveness probes, because the protocol health route requires the outbound bearer token and Kubernetes HTTP probes cannot safely resolve a Secret into an HTTP header.

A `PodDisruptionBudget` is deliberately **not** included. With one replica, a single RWO volume, and SQLite, `minAvailable: 1` would block voluntary maintenance without adding availability. Plan a maintenance window or move durable state to a multi-writer architecture before scaling beyond one replica.

The Cloudflare Quick Tunnel flow below is for live validation, not a production ingress. Use a stable, access-controlled HTTPS endpoint or named tunnel for long-lived use.

## Repository layout

| Path | Purpose |
| --- | --- |
| `Dockerfile` | Reproducible static multi-stage image build |
| `deploy/` | One-replica AKS base; Secrets are created from local files |
| `deploy/fixtures/` | Orka Gateway and echo `AgentRuntime` live fixture |
| `scripts/live-validate.sh` | Readiness/protocol validation skeleton requiring an explicit `KUBE_CONTEXT` |
| `.env.example` | Local file-based configuration example |

## Configuration

Non-secret settings are in `deploy/configmap.yaml`:

| Variable | Default | Notes |
| --- | --- | --- |
| `LISTEN_ADDRESS` | `:8080` | Adapter listener |
| `DATABASE_PATH` | `/data/telegram-adapter.db` | SQLite database on the PVC |
| `TELEGRAM_API_BASE_URL` | `https://api.telegram.org` | Override only for a trusted test endpoint |
| `TELEGRAM_WEBHOOK_URL` | empty | Full public callback URL, including `/telegram/webhook` |
| `TELEGRAM_DROP_PENDING_UPDATES` | `false` | Passed to Telegram when registering the webhook |
| `ORKA_GATEWAY_INGRESS_URL` | Orka API Gateway events URL | Must be the full `/api/v1/gateways/{namespace}/{name}/events` URL |
| `TELEGRAM_CONFORMANCE_CHAT_ID` | `0` | Optional chat used by an explicit delivery conformance check |
| `REQUEST_TIMEOUT` | `15s` | Positive Go duration |
| `SHUTDOWN_TIMEOUT` | `20s` | Positive Go duration |

The deployment supplies these supported `*_FILE` variables:

- `TELEGRAM_BOT_TOKEN_FILE`;
- `TELEGRAM_WEBHOOK_SECRET_FILE`;
- `ORKA_GATEWAY_INBOUND_TOKEN_FILE`;
- `ORKA_GATEWAY_OUTBOUND_TOKEN_FILE`.

Never set both a secret variable and its corresponding `*_FILE` variable. The inbound and outbound Orka bearer tokens must be different.

## Build and test

The default command package is `./cmd/orka-gateway-telegram`.

```bash
make check
make build
```

Build a local image with the default local repository name:

```bash
TAG=dev make image
```

### Build and push with buildx

Authenticate Docker to the registry without placing credentials in this repository. By default, `make image-push` uses the active buildx builder. Inspect that builder, then push an immutable tag:

```bash
docker buildx inspect --bootstrap

IMAGE=registry.example.com/example/orka-gateway-telegram \
TAG="$(git rev-parse --short=12 HEAD)" \
PLATFORMS=linux/amd64 \
make image-push

IMAGE=registry.example.com/example/orka-gateway-telegram \
TAG="$(git rev-parse --short=12 HEAD)" \
make inspect-image
```

To select a different configured builder, add `BUILDER=my-builder` to the `make image-push` invocation. Set `PLATFORMS=linux/amd64,linux/arm64` only when every target cluster architecture is required. The pushed image includes BuildKit provenance and an SBOM. Docker's buildx reference documents builder selection, platform, and `--push` behavior: <https://docs.docker.com/reference/cli/docker/buildx/build/>.

## AKS prerequisites

The examples assume:

- `KUBE_CONTEXT` explicitly names the target AKS kubeconfig context;
- Orka and the `core.orka.ai` / `gateway.orka.ai` CRDs are installed;
- the Orka API Service is `orka-api.orka-system.svc.cluster.local:8080`;
- the AKS cluster has the `managed-csi` Azure Disk StorageClass;
- `kubectl`, `jq`, `envsubst`, `cloudflared`, `curl`, and Docker buildx are installed;
- a Telegram bot and a private chat/user ID are available.

`KUBE_CONTEXT` must be set; cluster commands do not fall back to the current context. Set it in every shell used for the examples below. For example:

```bash
export KUBE_CONTEXT="my-aks-context"

kubectl --context "${KUBE_CONTEXT}" cluster-info
kubectl --context "${KUBE_CONTEXT}" get crd \
  gatewayclasses.gateway.orka.ai \
  gateways.gateway.orka.ai \
  gatewaybindings.gateway.orka.ai \
  agentruntimes.core.orka.ai \
  agents.core.orka.ai
kubectl --context "${KUBE_CONTEXT}" get storageclass managed-csi
```

## Create Secrets without putting values in manifests

The safer workflow is to keep mode-`0600` source files outside the repository and stream generated Secret manifests directly to the API server.

```bash
export SECRET_DIR="${HOME}/.config/orka-gateway-telegram"
umask 077
mkdir -p "${SECRET_DIR}"

# Save the BotFather token manually without echoing it to the terminal.
${EDITOR:-vi} "${SECRET_DIR}/telegram-bot-token"

openssl rand -hex 32 >"${SECRET_DIR}/telegram-webhook-secret"
openssl rand -hex 32 >"${SECRET_DIR}/orka-gateway-inbound-token"
openssl rand -hex 32 >"${SECRET_DIR}/orka-gateway-outbound-token"
openssl rand -hex 32 >"${SECRET_DIR}/echo-runtime-token"
chmod 0600 "${SECRET_DIR}"/*
```

Create the namespace first:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" apply -f deploy/namespace.yaml
```

Create adapter and Gateway Secrets from files. Values are never passed as command-line literals:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  create secret generic telegram-adapter-secrets \
  --from-file=telegram-bot-token="${SECRET_DIR}/telegram-bot-token" \
  --from-file=telegram-webhook-secret="${SECRET_DIR}/telegram-webhook-secret" \
  --dry-run=client --output=yaml | \
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  create secret generic telegram-gateway-inbound \
  --from-file=token="${SECRET_DIR}/orka-gateway-inbound-token" \
  --dry-run=client --output=yaml | \
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  label secret telegram-gateway-inbound \
  gateway.orka.ai/inbound-auth=true \
  gateway.orka.ai/gateway-name=telegram \
  --overwrite
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  annotate secret telegram-gateway-inbound \
  gateway.orka.ai/gateway-name=telegram \
  --overwrite

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  create secret generic telegram-gateway-outbound \
  --from-file=token="${SECRET_DIR}/orka-gateway-outbound-token" \
  --dry-run=client --output=yaml | \
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  label secret telegram-gateway-outbound \
  gateway.orka.ai/outbound-auth=true \
  gateway.orka.ai/gateway-name=telegram \
  --overwrite
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  annotate secret telegram-gateway-outbound \
  gateway.orka.ai/gateway-name=telegram \
  --overwrite
```

Create the echo runtime bearer Secret and bind it to the exact runtime name and endpoint:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  create secret generic telegram-echo-runtime-token \
  --from-file=token="${SECRET_DIR}/echo-runtime-token" \
  --dry-run=client --output=yaml | \
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  label secret telegram-echo-runtime-token \
  orka.ai/agent-runtime-auth=true \
  orka.ai/agent-runtime-name=telegram-echo-runtime \
  --overwrite
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  annotate secret telegram-echo-runtime-token \
  orka.ai/agent-runtime-endpoint=http://telegram-echo-runtime.orka-gateway-telegram.svc.cluster.local:8080 \
  --overwrite
```

Do not use `kubectl get secret ... -o yaml`, paste tokens into shell history, or commit rendered Secret manifests. Rotate inbound, outbound, and runtime tokens independently.

## Deploy the one-replica base

Use an immutable image tag. The Secret objects must exist before the Pod can start because they are projected as files.

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

export IMAGE=registry.example.com/example/orka-gateway-telegram
export TAG="$(git rev-parse --short=12 HEAD)"

KUBE_CONTEXT="${KUBE_CONTEXT}" \
NAMESPACE=orka-gateway-telegram \
IMAGE="${IMAGE}" \
TAG="${TAG}" \
make deploy

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  get deployment,pod,service,pvc
```

The PVC is retained while the namespace exists. Back up the SQLite database with an application-consistent/WAL-consistent procedure before destructive maintenance or rollback.

## Cloudflare Quick Tunnel over a local port-forward

Orka only sends its outbound bearer token to an HTTPS adapter endpoint. The ClusterIP Service is plain HTTP, so the live fixture uses a temporary Quick Tunnel as the HTTPS boundary.

Terminal 1—forward the AKS Service only to loopback:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  port-forward service/orka-gateway-telegram 18080:8080
```

Terminal 2—start a Quick Tunnel:

```bash
cloudflared tunnel --url http://127.0.0.1:18080 --no-autoupdate
```

Copy the generated `https://<random>.trycloudflare.com` URL without a trailing slash:

```bash
export TUNNEL_URL=https://replace-with-generated-host.trycloudflare.com
```

Quick Tunnels are ephemeral and have no production SLA. Cloudflare documents this command and its limits at <https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/do-more-with-tunnels/trycloudflare/>.

Bind Orka's outbound Secret to that exact endpoint:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  annotate secret telegram-gateway-outbound \
  gateway.orka.ai/adapter-endpoint="${TUNNEL_URL}" \
  --overwrite
```

Set the full Telegram callback URL and restart the adapter so it registers the new webhook:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

config_patch="$(jq -cn \
  --arg url "${TUNNEL_URL}/telegram/webhook" \
  '{data:{TELEGRAM_WEBHOOK_URL:$url}}')"
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  patch configmap orka-gateway-telegram \
  --type=merge \
  --patch "${config_patch}"
unset config_patch

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  rollout restart deployment/orka-gateway-telegram
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  rollout status deployment/orka-gateway-telegram --timeout=5m
```

## Install the Orka echo fixture

The fixture includes:

- cluster-scoped `GatewayClass/telegram-chat-live-c65ebad8`;
- namespaced `AgentRuntime/telegram-echo-runtime` and its deterministic echo harness Service;
- namespaced `Agent/telegram-echo` selecting that runtime;
- `Gateway/telegram` pointing at the Quick Tunnel HTTPS endpoint;
- `GatewayBinding/telegram-echo` matching one exact bot, private chat, and sender.

The adapter normalizes Telegram identities as follows:

| Orka field | Telegram value |
| --- | --- |
| `accountId` | numeric bot ID |
| `contextId` | numeric private chat ID |
| `sender.id` | numeric Telegram user ID |
| `replyTarget` | adapter-owned `tg:v1:...` destination |

For a bot token shaped as `<bot-id>:<secret>`, derive only the non-secret numeric bot ID from the protected token file. In a direct private bot conversation, the chat ID and sender ID are normally the same numeric user ID; verify them for the account used in the test.

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

export TELEGRAM_ACCOUNT_ID="$(cut -d: -f1 <"${SECRET_DIR}/telegram-bot-token")"
export TELEGRAM_CHAT_ID=REPLACE_WITH_NUMERIC_PRIVATE_CHAT_ID
export TELEGRAM_SENDER_ID=REPLACE_WITH_NUMERIC_TELEGRAM_USER_ID

kubectl --context "${KUBE_CONTEXT}" apply -k deploy/cluster
kubectl --context "${KUBE_CONTEXT}" apply -k deploy/fixtures

envsubst '${TUNNEL_URL} ${TELEGRAM_ACCOUNT_ID} ${TELEGRAM_CHAT_ID} ${TELEGRAM_SENDER_ID}' \
  <deploy/fixtures/gateway-and-binding.yaml.tmpl | \
kubectl --context "${KUBE_CONTEXT}" apply -f -
```

Wait for readiness:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" \
  wait --for=jsonpath='{.status.accepted}'=true \
  gatewayclass/telegram-chat-live-c65ebad8 --timeout=5m
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  wait --for=jsonpath='{.status.ready}'=true \
  agentruntime/telegram-echo-runtime --timeout=5m
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  wait --for=jsonpath='{.status.ready}'=true \
  gateway/telegram --timeout=5m
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  wait --for=jsonpath='{.status.ready}'=true \
  gatewaybinding/telegram-echo --timeout=5m
```

`GatewayClass` is cluster-scoped. Coordinate its name with other operators before applying or deleting the fixture in a shared cluster.

## Live validation

The script requires `KUBE_CONTEXT` and uses explicit `kubectl --context "${KUBE_CONTEXT}"` calls. It does not read Kubernetes Secret data. Give it the same local outbound-token file used to create `telegram-gateway-outbound`:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

KUBE_CONTEXT="${KUBE_CONTEXT}" \
ORKA_GATEWAY_OUTBOUND_TOKEN_FILE="${SECRET_DIR}/orka-gateway-outbound-token" \
./scripts/live-validate.sh
```

It checks:

1. the adapter Deployment, PVC, Service, and Secret object presence;
2. authenticated `/v1/health` and `/v1/capabilities` through a temporary local port-forward;
3. `GatewayClass`, `AgentRuntime`, `Gateway`, and `GatewayBinding` readiness.

Then send a private text message to the bot from the configured sender. Unsupported update kinds and non-private chats are intentionally ignored. Verify inbound and outbound activity without reading Secret values:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  get gatewaybinding/telegram-echo \
  -o jsonpath='{.status.lastInboundActivity}{" -> "}{.status.lastOutboundActivity}{"\n"}'

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  get tasks,sessions
```

## Operations

- Keep `replicas: 1` while SQLite is the durable store.
- Keep the Deployment strategy `Recreate` with the RWO volume.
- Use immutable image tags and record the image digest used for each rollout.
- Snapshot `/data/telegram-adapter.db` consistently with its WAL files before restore tests or destructive upgrades.
- Rotate the Telegram webhook secret by updating the Secret, restarting the adapter, and re-registering the webhook endpoint.
- Rotate the Orka inbound and outbound tokens separately; update the adapter and their bound Gateway Secrets in the same maintenance window, then restart and wait for the adapter. Restart the echo runtime after rotating its runtime token.
- Treat a changed Quick Tunnel URL as a new adapter endpoint: update `Gateway.spec.adapter.endpoint`, `TELEGRAM_WEBHOOK_URL`, and the outbound Secret `gateway.orka.ai/adapter-endpoint` annotation together; then restart the adapter and verify current-generation readiness.
- A stable production endpoint must terminate trusted HTTPS before traffic reaches the ClusterIP Service.

## Cleanup

Delete the Telegram webhook **before** stopping the Quick Tunnel. Use a mode-`0600` temporary curl config so the bot token is not placed in process arguments or shell history:

```bash
delete_webhook_config="$(mktemp)"
chmod 0600 "${delete_webhook_config}"
bot_token="$(tr -d '\r\n' <"${SECRET_DIR}/telegram-bot-token")"
printf 'url = "https://api.telegram.org/bot%s/deleteWebhook"\nrequest = "POST"\nsilent\nshow-error\nfail\n' \
  "${bot_token}" >"${delete_webhook_config}"
unset bot_token
curl --config "${delete_webhook_config}"
rm -f "${delete_webhook_config}"
```

Remove the live routing objects, then the static fixture:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  delete gatewaybinding/telegram-echo gateway/telegram \
  --ignore-not-found
kubectl --context "${KUBE_CONTEXT}" delete -k deploy/fixtures --ignore-not-found
# GatewayClass is cluster-scoped and intentionally retained; delete it only after confirming no other Gateway uses it.
kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  delete secret/telegram-echo-runtime-token --ignore-not-found
```

Stop `cloudflared` and the `kubectl port-forward` processes. To remove the adapter but preserve the PVC, delete the Deployment, Service, ConfigMap, ServiceAccount, and three adapter/Gateway Secrets individually. To destroy all namespaced data, including the RWO PVC and SQLite database:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" delete namespace orka-gateway-telegram
```

Finally remove the local secret directory only after confirming the tokens are rotated or no longer needed.

### In-cluster throwaway Quick Tunnel

For a throwaway validation that remains available without a local port-forward,
`deploy/fixtures/quick-tunnel.yaml` runs one pinned `cloudflared` replica in the
adapter namespace. Its generated hostname changes if the Pod is recreated.
After applying the fixture, reconcile the Telegram webhook, Gateway endpoint,
and outbound Secret endpoint binding together:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" apply -k deploy/fixtures
KUBE_CONTEXT="${KUBE_CONTEXT}" ./scripts/reconcile-quick-tunnel.sh
```

This keeps the webhook configured; it does not call `deleteWebhook`. A named
Cloudflare Tunnel or a normal ingress/DNS certificate is required for a stable
production hostname.

## Real AI responses through Vekil

The deterministic echo runtime intentionally returns the terminal result `ok`.
For real responses, create an Agent that uses the in-cluster Vekil OpenAI-compatible
endpoint and point the GatewayBinding at that Agent.

Create the non-sensitive Vekil client configuration Secret in the Gateway namespace:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" --namespace orka-gateway-telegram \
  create secret generic telegram-vekil-gpt56 \
  --from-literal=OPENAI_API_KEY=dummy \
  --from-literal=OPENAI_BASE_URL=http://vekil.vekil-system.svc:1337/v1 \
  --dry-run=client --output=yaml | \
kubectl --context "${KUBE_CONTEXT}" apply -f -
```

Apply the conversational Agent:

```bash
: "${KUBE_CONTEXT:?Set KUBE_CONTEXT to the intended cluster context}"

kubectl --context "${KUBE_CONTEXT}" apply -k deploy/ai
```

The Agent sets `defaultAllowBash: true` because Orka requires that capability for
the Codex CLI runtime. The current Vekil fixture proxies model inference but not
Codex's `/v1/alpha/search` endpoint, so the system prompt explicitly disables live
web claims, blocks shell-based network retrieval, treats pasted source text as
untrusted data, asks for that text when freshness matters, and identifies the
configured model accurately when asked.

Render `deploy/ai/gatewaybinding.yaml.tmpl` with the normalized Telegram account,
chat, and sender IDs, then apply it. The live binding is named `telegram-ai` and
uses `gpt-5.6-sol` through Vekil.
