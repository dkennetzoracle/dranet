# OKE BM.GPU.GB300.4 multi-planar RoCEv2 with DraNet

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

Rails 0-1 are on NUMA 0, rails 2-3 on NUMA 1. Each rail is its own /64 with its own
router, and the prefix differs per node — the fabric is routed, not a flat L2.

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

## Two ways to hand a rail to a Pod

**Passthrough** (`resource-claim-template.yaml`) moves the rail PF into the Pod namespace.
**IPVLAN** (`resource-claim-template-ipvlan.yaml`) leaves the PF on the host and gives the
Pod an IPVLAN child of it. Both were measured on this cluster, 2 nodes x 4 GPUs = 8 ranks:

| | passthrough | IPVLAN |
|---|---|---|
| `RunPodSandbox`, 4 NICs | **4.4 - 5.7 s** | **8 - 88 ms** |
| vs. the runtime's ~2s plugin budget | exceeds it; later NICs attach past the deadline | nowhere near it |
| PF after the Pod exits | **DOWN, unaddressed** — the rail stays dark until something brings it up | **UP, still addressed** — the child just disappears |
| Pods per rail | 1 | many |
| RDMA netns mode required | exclusive or shared | **shared only** |
| `NCCL_IB_GID_INDEX` | 3 | **7** |
| `all_reduce_perf` busbw @ 8 GiB | 166.10 GB/s | 166.10 GB/s |

IPVLAN costs nothing in bandwidth and removes both operational problems. Prefer it unless
you need the RDMA device itself isolated into the Pod.

### IPVLAN requires shared RDMA netns mode

DraNet rejects a subinterface claim on an RDMA device in exclusive mode:

```
device pci-0000-03-00-0: interface type "IPVLAN" (subinterface) is not supported
with exclusive RDMA mode; use shared RDMA mode
```

and the requirement is real, not just a DraNet check: in exclusive mode the kernel does not
expose an IPVLAN child's GIDs inside the Pod namespace at all, so there is nothing for
`NCCL_IB_GID_INDEX` to select.

```
rdma system set netns shared
```

Two things to know before doing this:

* **It only works when the node has no non-init network namespaces.** Every one registers
  in the kernel's `rdma_nets`, and `rdma system set` returns `EBUSY` while any exist.
  `kubectl drain --ignore-daemonsets` is not enough, because the DaemonSets that keep their
  own namespace (the gpu-operator set, node-feature-discovery) stay. In practice: set it
  from a boot-time unit ordered before the container runtime, or temporarily withdraw those
  DaemonSets from the node (for gpu-operator, flip its `nvidia.com/gpu.deploy.*` node labels
  to `false`), flip, then restore. The setting is not persistent, so a reboot returns the
  node to `exclusive`.
* **Shared mode gives up per-Pod RDMA device isolation.** Every Pod sees every RDMA device
  on the node (`mlx5_0` … `mlx5_17`), so pin NCCL to the claimed rails with an exact-match
  list: `NCCL_IB_HCA==rdma_rail0,rdma_rail1,rdma_rail2,rdma_rail3`. The **GID tables remain
  namespace-filtered**, so a Pod still cannot use another Pod's or the host's address.

DraNet reads the mode once at startup, so restart its Pods after changing it:

```
kubectl -n kube-system logs -l app=dranet | grep "RDMA subsystem in mode"
```

### Why the GID index differs

The GID table is filtered per namespace, and the indices are absolute. The host PF's four
GIDs occupy 0-3, so a Pod sees zeros there and its own child's entries above them:

```
                        in the Pod                      on the host
gid[0..3]   0000:...:0000                    fe80::a20:e7ff:fe96:708   (PF, v1/v2)
                                             fdcd:8200:cde5:20b7:a20:e7ff:fe96:708
gid[4,5]    fe80::820:e700:196:708  v1/v2    0000:...:0000
gid[6,7]    fdcd:8200:cde5:20b7:820:e700:196:708  v1/v2
```

`gid[7]` is the RoCE v2 routable GID, uniformly on all four rails. Check it rather than
assuming:

```
cat /sys/class/infiniband/rdma_rail0/ports/1/gid_attrs/types/7   # -> RoCE v2
cat /sys/class/infiniband/rdma_rail0/ports/1/gids/7
```

## Addressing

Every rail interface gets both its address and its default route from a Router
Advertisement:

```
rdma_p0_rail0  inet6 fdcd:8200:cde5:20b7:a20:e7ff:fe96:708/64 dynamic mngtmpaddr proto kernel_ra
default via fe80::b061:4eff:fe0c:d0b7 dev rdma_p0_rail0 proto ra metric 1024 expires 1536sec
```

**Passthrough** uses `addressing: SLAAC`, which inherits no host addresses, leaves out the
`proto ra` routes the Pod re-learns, sets the per-interface IPv6 sysctls the kernel resets
on a namespace move, and waits for the resulting address before the workload starts. See
[interface configuration](https://dranet.dev/docs/user/interface-configuration/).

**IPVLAN** uses `addressing: Unnumbered` and lets each child autoconfigure from the RA. A
child shares its parent's MAC, but the kernel still derives a distinct interface identifier
per child, so there is no collision with the parent or between Pods:

```
rdma_p0_rail0 (PF, host)   fdcd:8200:cde5:20b7:0a20:e7ff:fe96:0708
child, first Pod           fdcd:8200:cde5:20b7:0820:e700:0196:0708
child, second Pod          fdcd:8200:cde5:20b7:0820:e700:0296:0708
```

`addressing: SLAAC` would express this intent better and would add the readiness wait, but
the subinterface path does not apply per-interface sysctls yet
([#326](https://github.com/kubernetes-sigs/dranet/pull/326)); until that lands, `SLAAC` is
rejected for subinterface types.

## Files

| File | Description |
|---|---|
| `deviceclass.yaml` | `DeviceClass` for `deviceClassName: dra.net` |
| `resource-claim-template.yaml` | `4rail` / `1nic-slaac` — passthrough, `addressing: SLAAC` |
| `resource-claim-template-ipvlan.yaml` | `4rail-ipvlan` — IPVLAN children, `addressing: Unnumbered` |
| `resource-claim-template-ipvlan-16plane.yaml` | `mp16-ipvlan` — all 16 planes as IPVLAN children |
| `mpi-job.yaml` | `all_reduce_perf`, 2 workers x 4 GPUs, passthrough claims |
| `mpi-job-ipvlan.yaml` | the same over IPVLAN claims (`NCCL_IB_GID_INDEX=7`) |
| `mpi-job-ipvlan-16plane.yaml` | all 16 planes with the OCI multi-planar tuning profile |

## Usage

```
kubectl apply -f deviceclass.yaml
kubectl apply -f resource-claim-template.yaml          # or -ipvlan.yaml
kubectl apply -f mpi-job.yaml                          # or mpi-job-ipvlan.yaml
kubectl logs -f -l training.kubeflow.org/job-role=launcher
```

Both jobs set `NCCL_MNNVL_ENABLE=0` deliberately. These nodes share one NVLink clique
(`nvidia-smi -q` reports the same `CliqueId` on both), and with MNNVL enabled NCCL carries
inter-node traffic over NVLink and never touches the rail NICs — the run would prove nothing
about the fabric. Set it back to `1` for the rack-scale number, which additionally needs the
IMEX channel devices in the Pod: `NVIDIA_IMEX_CHANNELS` alone did not inject them here, so
use the NVIDIA DRA driver's `ComputeDomain`.

GPUs come from the NVIDIA device plugin (`nvidia.com/gpu`) here, not from DRA. If the NVIDIA
DRA driver is installed, add a `gpu.nvidia.com` request to the claim template instead.

## Measured results

2 x `BM.GPU.GB300.4`, 8 ranks, `all_reduce_perf -b 8 -e 8G -f 2 -g 1 -c 0`, NCCL 2.29.3,
`NCCL_MNNVL_ENABLE=0` so the RoCE path is exercised, 4 rails per node
(`rdma_p0_rail0..3`). `Out of bounds values: 0 OK` in both runs.

| size | passthrough busbw | IPVLAN busbw |
|---|---|---|
| 1 MiB | 20.35 GB/s | 20.47 GB/s |
| 16 MiB | 66.33 GB/s | 66.23 GB/s |
| 256 MiB | 139.14 GB/s | 139.10 GB/s |
| 1 GiB | 159.13 GB/s | 159.14 GB/s |
| 4 GiB | 165.06 GB/s | 165.07 GB/s |
| 8 GiB | **166.10 GB/s** | **166.10 GB/s** |
| avg over all sizes | 48.292 GB/s | 48.350 GB/s |

Both runs report `NET/IB : Using [0]rdma_rail0:1/RoCE [1]rdma_rail1:1/RoCE
[2]rdma_rail2:1/RoCE [3]rdma_rail3:1/RoCE [RO]` and `Using network IB`, confirming the
traffic went over the claimed rails rather than NVLink.

### All 16 planes, OCI multi-planar tuning profile

`mpi-job-ipvlan-16plane.yaml`, same 2 nodes and 8 ranks,
`all_reduce_perf -b 1G -e 16G -f 2 -g 1 -n 50`, correctness checking on
(`validation: 1`, 0 wrong values in every row), `Out of bounds values: 0 OK`:

| size | busbw | algbw |
|---|---|---|
| 1 GiB | 349.77 GB/s | 199.87 GB/s |
| 2 GiB | 355.92 GB/s | 203.38 GB/s |
| 4 GiB | 359.13 GB/s | 205.22 GB/s |
| 8 GiB | 360.81 GB/s | 206.17 GB/s |
| 16 GiB | **361.61 GB/s** | 206.63 GB/s |
| avg | 357.441 GB/s | |

Using all 16 planes is worth **2.2x** over 4: 360.81 GB/s against 166.10 GB/s at 8 GiB.
NCCL selected all sixteen —
`NET/IB : Using [0]rdma_rail0_dma ... [15]mlx5_12_dma [RO]` — and the comm reports
`nNodes 2 localRanks 4 MNNVL 0`, so the traffic genuinely crossed the fabric rather than
NVLink.

The attach cost stays negligible at that width:

| | 4 planes | 16 planes |
|---|---|---|
| `PrepareResourceClaim` (host side, unbudgeted) | 48 ms | 465 ms |
| `RunPodSandbox` (runtime hook, ~2s budget) | 8 - 88 ms | **28.6 ms** |

### NVLS and MNNVL do not work in a Pod here

Both are in the OCI profile and both fail, with the same CUDA error in different
transports:

```
NVLS=1    transport/nvls.cc:91  NCCL WARN Cuda failure 801 'operation not supported'
MNNVL=1   transport/p2p.cc:281  NCCL WARN Cuda failure 801 'operation not supported'
```

MNNVL gets much further: with `/dev/nvidia-caps-imex-channels` hostPath-mounted into the
Pod, `nvidia-smi` reports Fabric `Completed`/`Success`, `CliqueId 3666`, `Healthy`, NCCL
logs `MNNVL 1 cliqueId e52 cliqueSize 8` and builds channels `via P2P/MNNVL` — and only
then fails to map remote memory. Without the mount it does not start, reporting `MNNVL is
available but not working on this system`.

So the device node is necessary but not sufficient: both features need a provisioned IMEX
domain, which is what the NVIDIA DRA driver's `ComputeDomain` is for. It is not installed
on this cluster.

Worth knowing even once it is: with MNNVL working, NCCL collapsed the two physical nodes
into `nNodes 1 localRanks 8` and stopped using the NICs altogether. An MNNVL run therefore
does not measure the rail fabric — keep `NCCL_MNNVL_ENABLE=0` for that.
