# upbot-operator
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/upbot-operator)](https://artifacthub.io/packages/search?repo=upbot-operator)
![GitHub Repo stars](https://img.shields.io/github/stars/upbothq/upbot-operator)

Kubernetes operator for [Upbot](https://upbot.app). Declare uptime monitors as `Monitor` resources next to your
workloads, or let the operator create one for each Ingress, and it keeps them in sync with your Upbot account:
created with the resource, updated with its spec, deleted with it.

## Install

You need an Upbot API key: in the Upbot dashboard, open **Settings → API** and create a token (the API comes with
paid plans).

```sh
kubectl create namespace upbot-operator-system
kubectl -n upbot-operator-system create secret generic upbot-api-key --from-literal=token=<your API key>

helm install upbot-operator oci://ghcr.io/upbothq/charts/upbot-operator \
  --namespace upbot-operator-system
```

Add `--set upbot.ingressWatcher.enable=true` to monitor every Ingress of the cluster (see below). The chart is also
published as a Helm repository, `https://upbothq.github.io/upbot-operator`, and on
[Artifact Hub](https://artifacthub.io/packages/search?repo=upbot-operator).

## Monitors

```yaml
apiVersion: monitoring.upbot.app/v1alpha1
kind: Monitor
metadata:
  name: api
  namespace: shop
spec:
  type: http                        # http, ping, tcp or dns
  target: https://api.example.com   # a URL for http, a host for ping and dns, host:port for tcp
  interval: "60"                    # seconds: 30, 60, 120, 300 or 600
```

The monitor appears in Upbot as `shop/api`. `kubectl get monitors` shows whether it is synced:

```console
$ kubectl get monitors -A
NAMESPACE   NAME   TYPE   TARGET                    INTERVAL   READY   ID                                     AGE
shop        api    http   https://api.example.com   60         True    0199d811-f889-7130-a91f-62fd291c06d6   3m
```

When the API refuses a monitor (invalid target, plan limit, revoked key), `READY` is `False` and the API's error is
on the Ready condition: `kubectl describe monitor api -n shop`, or `kubectl get monitors -o wide` for the reason.

The operator owns the monitors it creates:

- Deleting the `Monitor` deletes the monitor in Upbot.
- Every 10 minutes it reads each monitor back from Upbot: changes made in the dashboard are undone and a monitor
  deleted there is created again. Change the period with `upbot.resyncPeriod`, `"0"` turns it off.
- Monitors created in the Upbot dashboard are never touched.

## Ingress watcher

With `upbot.ingressWatcher.enable=true`, the operator creates an http monitor for each host of each Ingress, and
deletes it with the Ingress. Annotations on the Ingress change what is monitored:

| Annotation           | Effect                                                  |
| -------------------- | ------------------------------------------------------- |
| `upbot.app/monitor`  | `"false"` skips the Ingress and deletes its monitors    |
| `upbot.app/path`     | path to check, for example `/health`                    |
| `upbot.app/interval` | interval in seconds, instead of `upbot.ingressWatcher.interval` |

See [the annotations guide](docs/INGRESS_WATCHER_ANNOTATIONS.md) for the details.

## Chart values

| Value                              | Default         | Description                                                        |
| ---------------------------------- | --------------- | ------------------------------------------------------------------ |
| `upbot.apiKeyExistingSecret`       | `upbot-api-key` | Secret with the API key under `token`                              |
| `upbot.apiKey`                     |                 | API key, the chart then creates the Secret (avoid in production)   |
| `upbot.resyncPeriod`               | `10m`           | How often monitors are compared with Upbot, `"0"` turns it off     |
| `upbot.ingressWatcher.enable`      | `false`         | Create a monitor for each Ingress                                  |
| `upbot.ingressWatcher.interval`    | `60`            | Interval of the Ingress monitors                                   |
| `cleanup.enabled`                  | `true`          | On uninstall, delete the operator's monitors from Upbot            |
| `crd.keep`                         | `true`          | Keep the CRD when the chart is uninstalled                         |
| `prometheus.enable`                | `false`         | Create a ServiceMonitor for the operator's metrics                  |

All values are in [`dist/chart/values.yaml`](dist/chart/values.yaml).

## Uninstall

```sh
helm uninstall upbot-operator --namespace upbot-operator-system
```

Before the operator is removed, a cleanup job deletes every `Monitor`, so the operator deletes their monitors from
Upbot. Monitors it could not delete within `cleanup.timeout` are listed in the job's logs. Set
`cleanup.enabled=false` to keep the monitors in Upbot.

## Development

Requires Go (version in `go.mod`), Docker and [kind](https://kind.sigs.k8s.io/) for the e2e tests.

```sh
make test          # unit tests, against a local API server (envtest)
make lint          # golangci-lint
make test-e2e      # e2e tests in a kind cluster
make run           # run the operator against your current kubectl context, needs UPBOT_TOKEN
```

After changing `api/`, run `make manifests generate`: it regenerates the CRD, the chart's copy of it and the
DeepCopy code. CI fails when they are not committed.

Releases are made by a GitHub workflow, see [docs/VERSION_MANAGEMENT.md](docs/VERSION_MANAGEMENT.md).

## License

Apache License 2.0.
