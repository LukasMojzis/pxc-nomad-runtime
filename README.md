# PXC Nomad Runtime

Small runtime helpers for running Percona XtraDB Cluster and `garbd` under
Nomad with Consul service discovery.

The `pxc-runtime` binary resolves Galera peers from Consul DNS SRV records,
starts PXC or `garbd`, and provides simple health-check commands for Nomad
service checks.

Images:

- `ghcr.io/lukasmojzis/pxc-nomad-pxc-runtime:8.4`
- `ghcr.io/lukasmojzis/pxc-nomad-garbd-runtime:8.4`
- `ghcr.io/lukasmojzis/pxc-nomad-control:latest`
- `ghcr.io/lukasmojzis/pxc-nomad-node-classifier:latest`

`pxc-nomad-node-classifier` measures local host facts and applies Nomad node
metadata such as `pxc_member_role`, `pxc_admission`, and
`pxc_storage_status`. Jobs should use that metadata for placement instead of
hardcoding site or datacenter names.
