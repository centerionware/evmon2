# Evmon

**Evmon** is a highly opinionated, small, and fast Kubernetes-native service monitor. It builds a list of services based on Ingress objects, targets the internal service inside the cluster, and probes it every 30 seconds. External hosts of the ingress are probed every 5 minutes to minimize external traffic.  

Evmon also provides CRDs for monitoring custom URLs.  

---

## Features

- Kubernetes-native, opinionated, and minimal footprint.
- Probes internal services every 30 seconds.
- Probes external URLs every 5 minutes.
- Supports custom URL monitoring via CRDs.
- Provides `/status` and `/history` endpoints accessible only to client applications that are registered (See below)
- Stores only changes in the database to minimize storage.
- Uses RBAC to ensure security within the cluster
- Lightweight and easy to deploy.
- Post Quantum encryption for client->backend communications

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
  url: https://github.com/centerionware/evmon2.git
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

1. Deploy Evmon using the Kustomization.  
2. Create `EvmonEndpoint` CRDs for any custom URLs you want to monitor.  
3. Retrieve the generated `ADMIN_KEY` secret:
```bash
kubectl -n evmon get secret evmon-admin-secret -o jsonpath="{.data.admin_key}" | base64 --decode
```
4. Use the `ADMIN_KEY` to call the `/create_client` endpoint:  
   - Include the key in the HTTP header `X-Admin-PSK`.  
   - This endpoint will generate a **client ID** and **client PSK**.  
   - Example:

```bash
curl -X POST https://<evmon-service>/create_client \
  -H "X-Admin-PSK: <ADMIN_KEY>" \
  -d '{"name":"frontend"}'
```
5. Provide the **client ID** and **client PSK** to the frontend or UI application (once developed).  
   - Only the UI app can use these credentials to securely access `/status` and `/history`.  
   - End users themselves **cannot directly access these endpoints**; the system is designed for secure client access only.  

> Note: The `ADMIN_KEY` is solely for creating clients. All subsequent access requires the client credentials, keeping Evmon’s endpoints secure and protected from direct human or script access.  

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

Evmon leverages Kubernetes RBAC to limit access and ensure security when accessing cluster resources.  

We use a quantum resistant asymmetrical algorithm for UI -> backend communications. Backend must be hosted behind an ingress with a valid SSL certificate to prevent MITM and protect from leaking any PSK's. 

A leak of a client id and PSK would result in unauthorized clients being able to register.
As of now it's HIGHLY untested. 
It _should_ in the end allow only one client per client_id with a PSK, client PSK's are all unique. There will eventually be a method to revoke access to a specific client id if it gets leaked.

---

## Contributing

- Current status: Feature Freeze, maintainance only. PR's will be considered if they fix things or add highly valuable functionality only if they've been fully tested. 
- Production deployment, PostgreSQL, and MariaDB support are untested.  
- Please open an issue if you test any of the production setups or databases.  

---

## Design Philosophy

KISS. This is meant to be a simple backend. There will never be a builtin UI or Notification utility. Those are meant to be seperate isolated components. 

---

## License

[MIT License](LICENSE)