# OKE BM.GPU.GB300.4 multi-planar RoCEv2 with SLAAC addressing

Topologically-aware RoCEv2 NIC allocation with
[DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on a multi-planar `BM.GPU.GB300.4` cluster, where every rail NIC takes its IPv6 address
from a Router Advertisement.

This is the multi-planar sibling of [`../BM.GPU.GB200-v3.4`](../BM.GPU.GB200-v3.4/). The
difference is the NIC layout and, because of it, the addressing.

## The hardware

Each node has 4 GPUs and 4 ConnectX-8 SuperNICs. Each SuperNIC exposes **4 plane PFs**, so
the kernel shows 16 RDMA netdevs named `rdma_p<plane>_rail<rail>`:

| netdev | PCI | RDMA device | NUMA | prefix |
|---|---|---|---|---|
| `rdma_p0_rail0` .. `rdma_p3_rail0` | `0000:03:00.0-.3` | `rdma_rail0`, `mlx5_1`, `mlx5_0`, `mlx5_2` | 0 | `fdcd:8200:cde5:20b7::/64` |
| `rdma_p0_rail1` .. `rdma_p3_rail1` | `0002:03:00.0-.3` | `rdma_rail1`, `mlx5_4`, `mlx5_3`, `mlx5_5` | 0 | `fdcd:8200:cee5:20b7::/64` |
| `rdma_p0_rail2` .. `rdma_p3_rail2` | `0010:03:00.0-.3` | `rdma_rail2`, `mlx5_7`, `mlx5_6`, `mlx5_9` | 1 | `fdcd:8200:cfe5:20b7::/64` |
| `rdma_p0_rail3` .. `rdma_p3_rail3` | `0012:03:00.0-.3` | `rdma_rail3`, `mlx5_10`, `mlx5_11`, `mlx5_12` | 1 | `fdcd:8200:d0e5:20b7::/64` |

Rails 0-1 are on NUMA 0, rails 2-3 on NUMA 1. Each rail is its own /64 with its own router.

## Addressing: why `SLAAC`

Every one of those 16 interfaces gets both its address and its default route from a Router
Advertisement:

```
rdma_p0_rail0  inet6 fdcd:8200:cde5:20b7:a20:e7ff:fe96:708/64 dynamic mngtmpaddr proto kernel_ra
default via fe80::b061:4eff:fe0c:d0b7 dev rdma_p0_rail0 proto ra metric 1024 expires 1536sec
```

That address is what populates the routable GID at index 3, which is what NCCL uses
(`NCCL_IB_GID_INDEX=3`). By default DraNet would copy the host's address into the Pod as a
static one; `addressing: SLAAC` instead has the Pod autoconfigure itself, and DraNet waits
for the result before the workload starts. Inside the Pod:

```
$ ip -6 addr show rdma_p0_rail0
    inet6 fdcd:8200:cde5:20b7:a20:e7ff:fe96:708/64 scope global dynamic mngtmpaddr proto kernel_ra
$ cat /sys/class/infiniband/rdma_rail0/ports/1/gids/3
fdcd:8200:cde5:20b7:0a20:e7ff:fe96:0708
```

See [interface configuration](https://dranet.dev/docs/user/interface-configuration/) for the
full behaviour, including the deadline and rollback.

## `--uplink-interfaces` is required here

DraNet keeps the host's default-gateway uplinks out of the inventory so a Pod cannot take
the node's connectivity away. On this shape all 16 rail NICs have a default route at metric
1024, so the detection reads them as one big ECMP uplink and excludes the entire fabric —
the node publishes no RDMA devices at all. Name the real uplink instead:

```yaml
args:
- /dranet
- --uplink-interfaces=eth0
```

## Files

| File | Description |
|---|---|
| `deviceclass.yaml` | `DeviceClass` for `deviceClassName: dra.net` |
| `resource-claim-template.yaml` | `4rail` (one plane per rail) and `1nic-slaac` (a single NIC, for checking autoconfiguration on its own) |
| `mpi-job.yaml` | `MPIJob` running `all_reduce_perf` across 2 workers x 4 GPUs |

## Usage

```
kubectl apply -f deviceclass.yaml
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml
kubectl logs -f -l training.kubeflow.org/job-role=launcher
```

`mpi-job.yaml` sets `NCCL_MNNVL_ENABLE=0` deliberately. These nodes share one NVLink clique
(`nvidia-smi -q` reports the same `CliqueId` on both), and with MNNVL enabled NCCL carries
inter-node traffic over NVLink and never touches the rail NICs. Disabling it forces the
`NET/IB` path, which is what exercises the fabric and the autoconfigured addresses. Set it
back to `1` for the rack-scale number.

GPUs come from the NVIDIA device plugin (`nvidia.com/gpu`) here, not from DRA. If the NVIDIA
DRA driver is installed, add a `gpu.nvidia.com` request to the claim template and a
`ComputeDomain` for the IMEX channels instead.
