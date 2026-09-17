# OKE BM.GPU.GB300.4 multi-planar RoCEv2 with DraNet

Topologically-aware RoCEv2 NIC allocation with
[DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on a multi-planar `BM.GPU.GB300.4` cluster, where every rail NIC takes its IPv6 address
and its default route from a Router Advertisement.

This is the multi-planar sibling of [`../BM.GPU.GB200-v3.4`](../BM.GPU.GB200-v3.4/). The
difference is the NIC layout and, because of it, the addressing.

## The hardware

Each node has 4 GPUs and 4 ConnectX-8 SuperNICs, one per rail. Each SuperNIC shows up in
the kernel three ways:

| netdev | PCI | RDMA device | role |
|---|---|---|---|
| `rdma<N>` (the same function also owns `rdma_p0_rail<N>`) | `<dom>:03:00.0` | `rdma_rail<N>` | the PF. One RDMA device whose planes are its ports: 4 `ACTIVE` at 200 Gb/s. SR-IOV capable, 1 VF. Carries no address. |
| `rdma_vf_rail<N>` | `<dom>:03:00.4` | `rdma_vf_rail<N>` | the SR-IOV VF, 200 Gb/s (`2X NDR`). Holds the rail's RA address and sits in VRF `vrf_r<N>`, whose table holds the rail's RA default route. **This is the device a Pod claims.** |
| `rdma_p1_rail<N>` .. `rdma_p3_rail<N>` | `<dom>:03:00.1-.3` | none | the other plane netdevs, aggregated by OVS-DOCA behind `br-rail<N>`. Not RDMA-capable, and netns-local: a claim for one fails at the namespace move with `EINVAL` and the Pod loops in `ContainerCreating`. |

| rail | PCI domain | VF | NUMA | prefix on this node |
|---|---|---|---|---|
| 0 | `0000` | `rdma_vf_rail0` | 0 | `fdcd:8200:cde5:20b7::/64` |
| 1 | `0002` | `rdma_vf_rail1` | 0 | `fdcd:8200:cee5:20b7::/64` |
| 2 | `0010` | `rdma_vf_rail2` | 1 | `fdcd:8200:cfe5:20b7::/64` |
| 3 | `0012` | `rdma_vf_rail3` | 1 | `fdcd:8200:d0e5:20b7::/64` |

Rails 0-1 are on NUMA 0, rails 2-3 on NUMA 1. Each rail is its own /64 with its own
router, and the prefix differs per node (`…:20b7` here, `…:20b6` on the second node): the
fabric is routed, not a flat L2.

DraNet publishes 32 devices per node. The VFs carry `dra.net/rdma: true`,
`dra.net/isSriovVf: true`, `dra.net/rdmaDevice: rdma_vf_rail<N>`, `dra.net/numaNode`, the
RA address in `dra.net/ipv6`, and the OKE provider's node attributes
(`oke.dra.net/shape: BM.GPU.GB300.4`, `oke.dra.net/rdmaFabricIpv6: true`,
`oke.dra.net/rdmaFabricPlanes: 4`, `rackId`, `hpcIslandId`, …). The provider's `oke-rdma`
profile is IPv4-only, so on this fabric it hands out no profile and the claim's own
configuration applies. That is what these examples are.

Select by `rdma == true` plus the name. It keeps the plane netdevs and the PF out, and
the PF is the VF's parent, not the addressed endpoint.

## `--uplink-interfaces`

DraNet keeps the host's default-gateway uplinks out of the inventory so a Pod cannot take
the node's connectivity away. Detection reads the main routing table. On this layout the
rails' RA default routes live in the per-rail VRF tables, the main table holds only
`eth0`'s default, and detection is right on its own: without the flag DraNet logs
`Excluded uplink interfaces and children: [eth0]` and publishes all 32 devices.

On the earlier layout of this shape, 16 plane PFs each with an RA default route in the
main table at metric 1024, detection read the fabric as one big ECMP uplink and excluded
all of it, so the node published no RDMA devices. If that happens, name the uplink:

```yaml
args:
- /dranet
- --uplink-interfaces=eth0
```

DraNet warns when it detects more than two uplinks, which is the symptom.

## Two ways to hand a rail to a Pod

**Passthrough** (`resource-claim-template.yaml`) moves the rail VF into the Pod namespace.
**IPVLAN** (`resource-claim-template-ipvlan.yaml`) leaves the VF on the host and gives the
Pod an IPVLAN child of it. Both were measured on this cluster, 2 nodes x 4 GPUs = 8 ranks:

| | passthrough | IPVLAN |
|---|---|---|
| `RunPodSandbox`, 4 NICs | **0.81 - 0.93 s**: all four VFs are attached first (≈0.2 s each), then checked; by then three already have their address and the last takes 25 ms | **18 - 96 ms** |
| vs. the runtime's ~2s plugin budget | fits; the budget left for each successive NIC shrinks 1.29 s → 0.60 s | nowhere near it |
| VF after the Pod exits | returned to the host: DraNet puts it back into its VRF, restores its MTU and brings it up; the RA re-addresses it within a second | **untouched**: UP, addressed, in its VRF; the child just disappears |
| Pods per rail | 1 | many |
| RDMA netns mode required | exclusive or shared | **shared only** |
| `NCCL_IB_GID_INDEX` | 3 | **7** |
| `all_reduce_perf` busbw @ 16 GiB | 177.07 GB/s | 176.85 GB/s |

IPVLAN costs nothing in bandwidth and removes both operational problems. Prefer it unless
you need the RDMA device itself isolated into the Pod.

Passthrough teardown deserves a word. When the sandbox stops, the network namespace is
already gone by the time the runtime calls DraNet's `StopPodSandbox` hook, so the kernel
returns the VF to the host by itself: down, with its sysctls reset and its VRF membership
lost. DraNet records the VF's master, MTU and PCI address before the move and restores
them at that point, whether the interface came back under its own name or was renamed by
the kernel, and the same restore runs when a failed `RunPodSandbox` rolls a VF back. On
this cluster all four VFs were back in their VRFs and re-addressed by the RA within a
second of the Pod's deletion, with no manual step.

### IPVLAN requires shared RDMA netns mode

DraNet rejects a subinterface claim on an RDMA device in exclusive mode:

```
device pci-0000-03-00-4: interface type "IPVLAN" (subinterface) is not supported
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
  on the node (`rdma_rail0..3`, `rdma_vf_rail0..3`, the uplink's `mlx5_*`), so pin NCCL to
  the claimed rails with an exact-match list:
  `NCCL_IB_HCA==rdma_vf_rail0,rdma_vf_rail1,rdma_vf_rail2,rdma_vf_rail3`. The **GID tables
  remain namespace-filtered**, so a Pod still cannot use another Pod's or the host's address.

DraNet reads the mode once at startup, so restart its Pods after changing it:

```
kubectl -n kube-system logs -l app=dranet | grep "RDMA subsystem in mode"
```

### Why the GID index differs

The GID table is filtered per namespace, and the indices are absolute. The host VF's four
GIDs occupy 0-3, so a Pod with an IPVLAN child sees zeros there and its own child's entries
above them. Read on both sides during a run:

```
                        in the Pod (IPVLAN child)                 on the host
gid[0..3]   0000:...:0000                                fe80::cd:f6ff:fe70:49cd   (VF, v1/v2)
                                                         fdcd:8200:cde5:20b7:cd:f6ff:fe70:49cd
gid[4,5]    fe80::2cd:f600:170:49cd  v1/v2               0000:...:0000
gid[6,7]    fdcd:8200:cde5:20b7:2cd:f600:170:49cd v1/v2
```

`gid[7]` is the RoCE v2 routable GID, uniformly on all four rails. With passthrough the
VF's RDMA device follows the netdev into the Pod and the table reads inside the Pod as it
does on the host, `gid[3]` routable, while the host now reads zeros for that VF. Check it
rather than assuming:

```
cat /sys/class/infiniband/rdma_vf_rail0/ports/1/gid_attrs/types/7   # -> RoCE v2
cat /sys/class/infiniband/rdma_vf_rail0/ports/1/gids/7
```

## Addressing

Every rail VF gets both its address and its default route from a Router Advertisement,
the latter into its VRF's table:

```
rdma_vf_rail0  inet6 fdcd:8200:cde5:20b7:cd:f6ff:fe70:49cd/64 dynamic mngtmpaddr proto kernel_ra
table 100: default via fe80::b061:4eff:fe0c:d0b7 dev rdma_vf_rail0 proto ra metric 1024
```

**Passthrough** uses `addressing: SLAAC`, which inherits no host addresses, leaves out the
`proto ra` routes the Pod re-learns, sets the per-interface IPv6 sysctls the kernel resets
on a namespace move (`accept_ra=2`, `dad_transmits=0`, `router_solicitation_delay=0`), and
waits for the resulting address before the workload starts. The claim status carries the
outcome:

```
$ kubectl get resourceclaim -o json | jq -r '.items[].status.devices[] | "\(.device) \(.networkData.interfaceName) \(.networkData.ips) \([.conditions[].type])"'
pci-0000-03-00-4 rdma_vf_rail0 ["fdcd:8200:cde5:20b7:cd:f6ff:fe70:49cd/64"] ["SLAACReady","Ready","NetworkReady"]
```

The wait took 25-26 ms per VF in every run here. If no advertisement arrives within the
budget the VF goes back to the host and the sandbox fails, so the Pod never starts on a
rail with no routable GID. See
[interface configuration](https://dranet.dev/docs/user/interface-configuration/) for the
deadline rules.

**IPVLAN** uses `addressing: Unnumbered` and lets each child autoconfigure from the RA. A
child shares its parent's MAC, but the kernel still derives a distinct interface identifier
per child, so there is no collision with the parent or between Pods:

```
rdma_vf_rail0 (VF, host)   fdcd:8200:cde5:20b7:00cd:f6ff:fe70:49cd
child, first Pod           fdcd:8200:cde5:20b7:02cd:f600:0170:49cd
```

The template also sets `acceptRA: 2`, `dadTransmits: 0` and `routerSolicitationDelay: 0`
on the child. The kernel starts a new child with the namespace defaults (`dad_transmits=1`,
a random delay of up to 1 s before the first solicitation), which puts the usable address
1-2 s after the Pod starts; with these three the child reads `accept_ra=2 dad_transmits=0
router_solicitation_delay=0` and has its address and `proto ra` default route by the time
anything looks. `addressing: SLAAC` is rejected for subinterfaces: the readiness wait and
its rollback are implemented for moved interfaces only.

## Files

| File | Description |
|---|---|
| `deviceclass.yaml` | `DeviceClass` for `deviceClassName: dra.net` |
| `resource-claim-template.yaml` | `4rail` / `1nic-slaac`: passthrough of the rail VFs, `addressing: SLAAC` |
| `resource-claim-template-ipvlan.yaml` | `4rail-ipvlan`: IPVLAN children of the rail VFs, `addressing: Unnumbered` plus the IPv6 sysctls |
| `compute-domain.yaml` | `ComputeDomain`: required for MNNVL and NVLS |
| `mpi-job.yaml` | `all_reduce_perf`, 2 workers x 4 GPUs, passthrough claims, `NCCL_IB_GID_INDEX=3` |
| `mpi-job-ipvlan.yaml` | the same over IPVLAN claims, `NCCL_IB_GID_INDEX=7` |
| `mpi-job-ipvlan-mnnvl.yaml` | the same with NVLS and MNNVL on: the NVLink path |

## Usage

```
kubectl apply -f deviceclass.yaml
kubectl apply -f resource-claim-template.yaml          # or -ipvlan.yaml
kubectl apply -f mpi-job.yaml                          # or mpi-job-ipvlan.yaml
kubectl logs -f -l training.kubeflow.org/job-role=launcher
```

The two RoCE jobs set `NCCL_MNNVL_ENABLE=0` and `NCCL_NVLS_ENABLE=0` deliberately. These
nodes share one NVLink clique (`nvidia-smi -q` reports the same `CliqueId` on both), and
with MNNVL enabled NCCL carries inter-node traffic over NVLink and never touches the rail
NICs; the run would prove nothing about the fabric. `mpi-job-ipvlan-mnnvl.yaml` turns both
back on for the rack-scale number, which additionally needs the `ComputeDomain`.

The NCCL environment in all three is the OCI multi-planar tuning profile without
`NCCL_TOPO_FILE` and the SPCX plugin, which this image does not carry. The workers run
unprivileged with `IPC_LOCK` only.

GPUs come from the NVIDIA device plugin (`nvidia.com/gpu`) here, not from DRA. If the NVIDIA
DRA driver is installed, add a `gpu.nvidia.com` request to the claim template instead.

## Measured results

2 x `BM.GPU.GB300.4`, 8 ranks, `all_reduce_perf -b 1G -e 16G -f 2 -g 1 -n 50`, correctness
checking on, NCCL 2.29.3, RDMA subsystem in shared mode on both nodes. Every run reported
`Out of bounds values: 0 OK`, `nNodes 2 localRanks 4 MNNVL 0`, and
`NET/IB : Using [0]rdma_vf_rail0_dma:1/RoCE [1]rdma_vf_rail1_dma:1/RoCE
[2]rdma_vf_rail2_dma:1/RoCE [3]rdma_vf_rail3_dma:1/RoCE [RO]`, so the traffic went over the
claimed VFs rather than NVLink.

| size | passthrough busbw | passthrough algbw | IPVLAN busbw | IPVLAN algbw |
|---|---|---|---|---|
| 1 GiB | 175.06 GB/s | 100.04 GB/s | 174.14 GB/s | 99.51 GB/s |
| 2 GiB | 176.06 GB/s | 100.60 GB/s | 175.54 GB/s | 100.31 GB/s |
| 4 GiB | 176.67 GB/s | 100.96 GB/s | 176.10 GB/s | 100.63 GB/s |
| 8 GiB | 176.85 GB/s | 101.06 GB/s | 176.22 GB/s | 100.69 GB/s |
| 16 GiB | **177.07 GB/s** | 101.18 GB/s | **176.85 GB/s** | 101.06 GB/s |
| avg | 176.359 GB/s | | 175.937 GB/s | |

Passthrough and IPVLAN are the same number, as they should be: an IPVLAN child is a
different address on the same VF.

**What the number means.** With two nodes, every all-reduce moves about one buffer's worth
of data out of each node (half in the reduce-scatter, half in the all-gather), so the
algorithm bandwidth is the inter-node bandwidth the job actually consumed: **101 GB/s
against 4 x 200 Gb/s = 100 GB/s of VFs**. The four VFs run at line rate. `busbw` is
`algbw x 2(n-1)/n`, 1.75x here, which is where 177 comes from. The SuperNICs have more
planes than this, but on this layout a Pod can only reach one VF per rail, so 100 GB/s per
node is the ceiling for a 4-VF claim regardless of tuning.

### NVLink path: NVLS and MNNVL, via ComputeDomain

`mpi-job-ipvlan-mnnvl.yaml`, the same job with `NCCL_NVLS_ENABLE=1` and
`NCCL_MNNVL_ENABLE=1`, which needs the `ComputeDomain` in `compute-domain.yaml`:

| size | NVLink busbw | NVLink algbw | RoCE (IPVLAN) busbw |
|---|---|---|---|
| 1 GiB | 720.73 GB/s | 411.85 GB/s | 174.14 GB/s |
| 2 GiB | 819.40 GB/s | 468.23 GB/s | 175.54 GB/s |
| 4 GiB | 828.28 GB/s | 473.30 GB/s | 176.10 GB/s |
| 8 GiB | 834.19 GB/s | 476.68 GB/s | 176.22 GB/s |
| 16 GiB | **839.37 GB/s** | 479.64 GB/s | **176.85 GB/s** |
| avg | 808.612 GB/s | | 175.937 GB/s |

**With MNNVL active NCCL stops using the NICs.** It collapses both physical nodes into one
communicator, `nNodes 1 localRanks 8 MNNVL 1`, and logs
`NVLS multicast support is available on dev N (NVLS_NCHANNELS 24)` and `nvlsRanks 8`. The
rail claim is carried but idle. An MNNVL run measures NVLink, not the fabric. Keep both
jobs.

### Why ComputeDomain is not optional

Without one, both features abort at init with the same CUDA error in different transports:

```
NVLS=1    transport/nvls.cc:91  NCCL WARN Cuda failure 801 'operation not supported'
MNNVL=1   transport/p2p.cc:281  NCCL WARN Cuda failure 801 'operation not supported'
```

The misleading part is how far MNNVL gets on a plain hostPath mount of
`/dev/nvidia-caps-imex-channels`: `nvidia-smi` reports the fabric `Completed`/`Success`,
one `CliqueId`, `Healthy`, and NCCL logs `cliqueSize 8` and builds channels
`via P2P/MNNVL`, then fails to map remote memory. Without the mount it does not get that
far, reporting `MNNVL is available but not working on this system`. So the device node is
necessary but not sufficient: the workload's GPUs must be authorised into an IMEX domain,
which is what the `ComputeDomain` does. It reports both nodes `Ready` under one cliqueID
and runs an IMEX daemon per node. Installing it did not change the RoCE result.

### The earlier layout of this shape, for reference

Before a re-provisioning on 2026-09-18 the same two nodes exposed each SuperNIC as 16
plane PFs, `rdma_p<plane>_rail<rail>`, each an RDMA device at 200 Gb/s with its own RA
address in the main table. The manifests for that layout are no longer in this directory
because they do not apply to the VF layout (planes 1-3 have no RDMA device now), but the
numbers are worth keeping next to the ones above, same nodes, same job:

| configuration | busbw @ 16 GiB | algbw @ 16 GiB |
|---|---|---|
| 4 plane-0 PFs, passthrough or IPVLAN, `-e 8G`, older NCCL settings | 166.10 GB/s | 94.9 GB/s |
| all 16 planes as IPVLAN children, this NCCL profile | 361.61 GB/s | 206.63 GB/s |
| NVLink (NVLS + MNNVL) | 839.19 GB/s | 479.5 GB/s |

Sixteen 200 Gb/s planes were worth 2.2x over four; the current layout's 4 VFs land within
7% of the old 4 PFs, the difference being the NCCL profile. Passthrough of four PFs took
4.4-5.7 s of `RunPodSandbox` against the ~2 s budget, which is what motivated the bounded
SLAAC wait; four VFs fit in under a second.
