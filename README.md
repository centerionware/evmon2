# Evmon

**Evmon** is a highly opinionated, small, and fast Kubernetes-native service monitor. It builds a list of services based on Ingress objects, targets the internal service inside the cluster, and probes it every 30 seconds. External hosts of the ingress are probed every 5 minutes to minimize external traffic.  

Evmon also provides CRDs for monitoring custom URLs.  

---

## Features

- Kubernetes-native, opinionated, and minimal footprint.
- Probes internal services every 30 seconds.
- Probes external URLs every 5 minutes.
- Supports custom URL monitoring via CRDs.
- Provides `/status` and `/history` endpoints.
- Stores only changes in the database to minimize storage.
- Uses RBAC to ensure security.
- Lightweight and easy to deploy.

---

## API Endpoints

- **`/status`** – Lists the status of all services currently monitored.  
- **`/history`** – Expects at least a `service_id` to show the history of a specific service.  
  - Stores only changes in the database.
  - Database: SQLite (default), can also use PostgreSQL or MariaDB (untested).  
  - For more details, see `internal/api.go`.

---

## Deployment

Evmon provides a Kustomize setup for example deployments.  

- **Development:** Fully working example is provided.  
- **Production:** Not yet tested.  
- **Database:** PostgreSQL and MariaDB support is untested; contributions or testing feedback are welcome via [issues](https://github.com/centerionware/evmon/issues).  

FluxCD users can deploy the dev version using the provided FluxCD example.

## FluxCD Example

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: evmon
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: evmon-repo
  namespace: evmon
spec:
  interval: 1m0s
  url: https://github.com/centerionware/evmon.git
  ref:
    branch: main
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: evmon-dev
  namespace: evmon
spec:
  interval: 5m0s
  path: ./kubernetes/overlays/dev
  prune: true
  sourceRef:
    kind: GitRepository
    name: evmon-repo
  validation: client
  namespace: evmon   # <-- Ensure all resources are created in this namespace
```

---

## Usage

1. Deploy Evmon using the kustomization.  
2. Create `EvmonEndpoint` CRDs for any custom URLs.  
3. Access `/status` to see the current state of services.  
4. Access `/history?service_id=<ID>` to see historical changes for a service.  

---

## Example CRD

```yaml
apiVersion: evmon.centerionware.com/v1
kind: EvmonEndpoint
metadata:
  name: google-homepage
  namespace: evmon
spec:
  url: https://google.com
  serviceID: Google
  intervalSeconds: 300
```

---

## Security

Evmon leverages Kubernetes RBAC to limit access and ensure security.  

---

## Contributing

- Current status: in active development.  
- Production deployment, PostgreSQL, and MariaDB support are untested.  
- Please open an issue if you test any of the production setups or databases.  

---

## License

[MIT License](LICENSE)