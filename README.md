# evmon

A kubernetes native cluster aware event monitoring suite. Think uptime Kuma for kubernetes. 

```yml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: evmon-repo
  namespace: flux-system
spec:
  interval: 1m0s
  url: https://github.com/centerionware/evmon
  branch: main
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: evmon-dev
  namespace: flux-system
spec:
  interval: 5m0s
  path: ./kubernetes/overlays/dev
  prune: true
  sourceRef:
    kind: GitRepository
    name: evmon-repo
  validation: client
```