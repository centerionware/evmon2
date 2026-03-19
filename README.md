# evmon

A kubernetes native cluster aware event monitoring suite. Think uptime Kuma for kubernetes. 

```yml
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
```