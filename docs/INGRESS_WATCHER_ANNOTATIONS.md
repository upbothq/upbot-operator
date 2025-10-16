# Ingress Watcher Annotations Guide

The Ingress Watcher automatically creates Monitor resources for your Ingress resources. You can customize the monitoring behavior using annotations on your Ingress resources.

## Supported Annotations

### `upbot.app/path`

**Purpose**: Append a custom path to the monitoring target URL.

**Example**:
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app
  annotations:
    upbot.app/path: "/health"
spec:
  rules:
  - host: myapp.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: my-app
            port:
              number: 80
```

**Result**: Monitor will check `https://myapp.example.com/health` instead of just `https://myapp.example.com`

**Path Handling**:
- Automatically adds leading `/` if missing
- Removes trailing `/` (except for root `/`)
- Examples:
  - `health` → `/health`
  - `/api/v1/` → `/api/v1`
  - `/` → `/`

### `upbot.app/interval`

**Purpose**: Override the default monitoring interval for this specific ingress.

**Example**:
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: critical-app
  annotations:
    upbot.app/interval: "10"  # Check every 10 seconds
spec:
  # ... ingress spec
```

**Behavior**:
- Takes precedence over the global `--ingress-watcher-interval` flag
- Must be a valid interval string (e.g., "10", "30", "60", "300")

### `upbot.app/monitor`

**Purpose**: Disable or control monitor creation for this ingress.

**Values**:
- `"false"` or `"disabled"`: Disable monitoring (will delete existing monitor)
- Any other value or absence: Enable monitoring (default)

**Example**:
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: internal-app
  annotations:
    upbot.app/monitor: "false"  # Don't monitor this ingress
spec:
  # ... ingress spec
```

**Behavior**:
- If set to `false` or `disabled`, any existing monitor will be deleted
- Useful for internal services or development environments

## Complete Example

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: production-api
  annotations:
    # Custom health check endpoint
    upbot.app/path: "/api/health"
    # Check every 15 seconds (critical service)
    upbot.app/interval: "15"
    # Monitoring is enabled (default, can be omitted)
    upbot.app/monitor: "true"
spec:
  tls:
  - hosts:
    - api.example.com
    secretName: api-tls
  rules:
  - host: api.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: api-service
            port:
              number: 8080
```

**Generated Monitor**:
- **Target**: `https://api.example.com/api/health`
- **Interval**: `15` seconds
- **Type**: `http`

## Labels and Annotations Added to Monitors

The Ingress Watcher adds metadata to created monitors for tracking:

### Labels
- `upbot.app/source: "ingress-watcher"` - Identifies monitors created by ingress watcher
- `upbot.app/target-type: "http"` - Indicates this is an HTTP monitor

### Annotations
- `upbot.app/auto-generated: "true"` - Marks as automatically generated
- `upbot.app/source-ingress: "namespace/ingress-name"` - Links to source ingress

## Behavior Details

### Monitor Updates
- When you change annotations, monitors are automatically updated on the next reconciliation
- Changes to `upbot.app/path` update the target URL
- Changes to `upbot.app/interval` update the monitoring frequency
- Changes to `upbot.app/monitor` can enable/disable monitoring

### Monitor Cleanup
- When an ingress is deleted, its monitor is automatically deleted
- When `upbot.app/monitor` is set to `false`/`disabled`, the monitor is deleted
- Only monitors created by the ingress watcher are managed (checked via labels)

### URL Generation
1. **Scheme**: `https` if TLS is configured, otherwise `http`
2. **Host**: First host from `spec.rules[0].host`
3. **Path**: Value from `upbot.app/path` annotation (if provided)

### Priority Order for Interval
1. `upbot.app/interval` annotation on the ingress (highest priority)
2. Global `--ingress-watcher-interval` flag
3. Default fallback: `"30"` seconds

## Troubleshooting

### Monitor Not Created
- Check that the ingress has at least one rule with a host
- Verify `upbot.app/monitor` is not set to `false` or `disabled`
- Check operator logs for errors

### Monitor Not Updated
- Ensure the monitor has the label `upbot.app/source: "ingress-watcher"`
- Manually created monitors are not managed by the ingress watcher
- Check if the annotation values are valid

### Wrong Target URL
- Verify the ingress has TLS configuration if you expect `https`
- Check the `upbot.app/path` annotation format
- Ensure the ingress rule has a valid host

## Best Practices

1. **Health Check Endpoints**: Use `upbot.app/path` to point to dedicated health check endpoints
   ```yaml
   annotations:
     upbot.app/path: "/health"
   ```

2. **Critical Services**: Use shorter intervals for important services
   ```yaml
   annotations:
     upbot.app/interval: "10"
   ```

3. **Development/Staging**: Disable monitoring for non-production environments
   ```yaml
   annotations:
     upbot.app/monitor: "false"
   ```

4. **API Services**: Monitor API health endpoints rather than the root path
   ```yaml
   annotations:
     upbot.app/path: "/api/v1/health"
     upbot.app/interval: "30"
   ```