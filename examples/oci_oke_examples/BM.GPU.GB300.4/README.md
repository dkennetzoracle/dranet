# OKE BM.GPU.GB300-v4 RoCEv2 DRANET Demo

End-to-end demo of topologically-aware GPU + RoCEv2 NIC allocation using
[Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
on Oracle Kubernetes Engine (OKE) with [BM.GPU.GB300.4](https://docs.oracle.com/en-us/iaas/Content/Compute/References/computeshapes.htm#bm-gpu) shapes.

This example is the GB300 sibling of [`../BM.GPU.GB200-v3.4`](../BM.GPU.GB200-v3.4/).
The structure is the same, but GB300.4 has half the physical RDMA NIC count
(4 vs 8) and uses SR-IOV virtual functions for pod-side RDMA traffic.

## Context

### Shape: BM.GPU.GB300.4

Each node has:

| Resource | Count | Detail |
|---|---|---|
| GPU | 4 x NVIDIA GB300 | 192 GB HBM3e, Blackwell-Ultra architecture, NVLink-18 all-to-all |
| NIC (PF) | 4 x Mellanox ConnectX-8 | 400 Gb/s RoCEv2 each |
| NIC (VF) | 4 x SR-IOV VFs | 1 VF per PF; this is what pods consume |
| NUMA nodes | 2 | 2 GPUs + 2 PFs (+ 2 VFs) per NUMA node |

> **Key difference from GB200:** GB200-v3.4 has **8** physical 400 Gb/s NICs
> per node (2 per GPU). GB300.4 has **4** physical NICs (1 per GPU), each
> exposing a single SR-IOV VF. Aggregate per-node NIC bandwidth is therefore
> half of GB200's — but at multi-node scale the busbw is dominated by the
> NVLink fabric (`NCCL_MNNVL_ENABLE=1`), not the IB NICs.

### Why VFs and not PFs?

On these GB300 nodes, the kernel exposes both the PFs (`rdma_rail0..3` on
PCI `x:03:00.0`) and one SR-IOV VF per PF (`rdma_vf_rail0..3` on PCI
`x:03:00.4`). `nvidia-smi topo -m` lists **only the VFs** as the GPU-visible
RDMA NICs (`NIC2..NIC5`), and only the VFs carry routable IPv6 addresses on
the OCI RDMA fabric. The DRA templates here select VFs via
`dra.net/sriov == false` (PFs have `sriov: true`, VFs have `sriov: false`).

### GPU-NIC topology

GPUs connect to the Grace CPU via NVLink C2C (chip-to-chip); NICs connect via
PCIe. `nvidia-smi topo -m` reports `SYS` for every GPU-NIC pair:

|      | GPU0 | GPU1 | GPU2 | GPU3 | NIC2 (vf0) | NIC3 (vf1) | NIC4 (vf2) | NIC5 (vf3) |
|------|------|------|------|------|-----|-----|-----|-----|
| GPU0 | X    | NV18 | NV18 | NV18 | SYS | SYS | SYS | SYS |
| GPU1 | NV18 | X    | NV18 | NV18 | SYS | SYS | SYS | SYS |
| GPU2 | NV18 | NV18 | X    | NV18 | SYS | SYS | SYS | SYS |
| GPU3 | NV18 | NV18 | NV18 | X    | SYS | SYS | SYS | SYS |

NIC mapping (VFs): NIC2=rdma_vf_rail0 (NUMA 0), NIC3=rdma_vf_rail1 (NUMA 0),
NIC4=rdma_vf_rail2 (NUMA 1), NIC5=rdma_vf_rail3 (NUMA 1).

NCCL enables GDR via `NCCL_NET_GDR_C2C=1` for NUMA-local NICs.

### DRA device attributes

**GPU** (driver: `gpu.nvidia.com`):

| Device | pciBusID | pcieRoot | NUMA |
|---|---|---|---|
| gpu-0 | 0008:06:00.0 | pci0008:00 | 0 |
| gpu-1 | 0009:06:00.0 | pci0009:00 | 0 |
| gpu-2 | 0018:06:00.0 | pci0018:00 | 1 |
| gpu-3 | 0019:06:00.0 | pci0019:00 | 1 |

**NIC** (driver: `dra.net`), VFs only:

| Device | ifName | pciAddress | NUMA | sriov | isSriovVf |
|---|---|---|---|---|---|
| pci-0000-03-00-4 | rdma_vf_rail0 | 0000:03:00.4 | 0 | false | true |
| pci-0002-03-00-4 | rdma_vf_rail1 | 0002:03:00.4 | 0 | false | true |
| pci-0010-03-00-4 | rdma_vf_rail2 | 0010:03:00.4 | 1 | false | true |
| pci-0012-03-00-4 | rdma_vf_rail3 | 0012:03:00.4 | 1 | false | true |

The CEL filter `dra.net/sriov == false && dra.net/rdma == true && dra.net/virtual == false`
selects exactly these four VFs (the PFs have `sriov: true`; eth1 has `sriov: true`
as well).

### OKE topology attributes

Same as GB200 — each NIC device carries node-level RDMA topology
(`oke.dra.net/hpcIslandId`, `networkBlockId`, `localBlockId`, `rackId`,
`gpuMemoryFabricId`). See [`../BM.GPU.GB200-v3.4/placement-group/`](../BM.GPU.GB200-v3.4/placement-group/)
for topology-aware scheduling — the same patterns apply on GB300.

### RoCEv2 and IPv6 on OKE

The ConnectX-8 VFs use RoCEv2; each carries a globally-routable IPv6 GUA
assigned by Router Advertisement on the host. This address populates a
routable GID in the VF's GID table at index 3, which NCCL uses
(`NCCL_IB_GID_INDEX=3`).

**Challenge:** In single-stack IPv4 Kubernetes clusters, the container
runtime sets `net.ipv6.conf.all.disable_ipv6=1` in pod namespaces. When
DRANET moves a VF into the pod, the host's IPv6 GUA cannot be applied
inside the pod (the kernel returns `EACCES`).

**DRANET behavior:** On `EACCES` while adding an IPv6 address, DRANET
enables IPv6 in the pod namespace (sets `net/ipv6/conf/all/disable_ipv6=0`
and the per-interface `disable_ipv6=0`) and retries the address application.
This populates the routable GID at index 3.

**NCCL configuration note:** `NCCL_IB_DATA_DIRECT=0` prevents NCCL from
selecting the Data Direct DMA interface and uses the standard IB verbs path
with the configured GID.

## Files

| File | Description |
|---|---|
| `resource-claim-template.yaml` | `ResourceClaimTemplate` objects: `1nic-aligned`, `1nic-unaligned`, `2nic-aligned`, `4gpu-4nic` |
| `mpi-job.yaml` | `MPIJob` running `all_reduce_perf` across 2 workers (per-node benchmarks) |
| `multinode-mpi-job.yaml` | `MPIJob` running `all_reduce_perf` across 16 workers with `4gpu-4nic` + ComputeDomain |
| `compute-domain.yaml` | `ComputeDomain` object that provisions IMEX channels for `NCCL_MNNVL_ENABLE=1` |
| `resourceslice-gpu.yaml` | Live GPU `ResourceSlice` from a GB300 node (reference) |
| `resourceslice-dranet.yaml` | Live NIC `ResourceSlice` from a GB300 node (reference) |

## Usage

```bash
# Install MPI Operator (if not already installed)
kubectl apply --server-side -k "https://github.com/kubeflow/mpi-operator/manifests/overlays/standalone?ref=v0.7.0"

# Apply ResourceClaimTemplates
kubectl apply -f resource-claim-template.yaml

# --- Per-node benchmarks (2 workers, 1 GPU/rank) ---
# Edit mpi-job.yaml resourceClaimTemplateName to: 1nic-aligned | 2nic-aligned | 1nic-unaligned
kubectl apply -f mpi-job.yaml
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=worker \
  --timeout=300s
kubectl logs -f $(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-dra,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')

# --- 16-node multinode benchmark (4 GPUs/rank, MNNVL enabled) ---
# Requires NVIDIA compute-domain.nvidia.com DRA driver on the cluster.
kubectl apply -f compute-domain.yaml
# Wait for the channel ResourceClaimTemplate to be created by the controller
kubectl wait --for=jsonpath='.metadata.name'=nccl-test-compute-domain-channel \
  resourceclaimtemplate/nccl-test-compute-domain-channel --timeout=30s 2>/dev/null || \
  kubectl get resourceclaimtemplate nccl-test-compute-domain-channel
kubectl apply -f multinode-mpi-job.yaml
kubectl wait --for=condition=ready pod \
  -l training.kubeflow.org/job-name=nccl-test-multinode,training.kubeflow.org/job-role=worker \
  --timeout=300s
kubectl logs -f $(kubectl get pods \
  -l training.kubeflow.org/job-name=nccl-test-multinode,training.kubeflow.org/job-role=launcher \
  -o jsonpath='{.items[0].metadata.name}')
```

## ResourceClaimTemplates

Four templates are defined. Update `mpi-job.yaml`
`resourceClaimTemplateName:` to switch between them.

The templates use NUMA-based CEL selectors. VFs are selected via
`dra.net/sriov == false` (since PFs have `sriov: true` and the VFs are what
the GPUs see).

### `1nic-aligned` — 1 GPU + 1 NIC, same NUMA

gpu-0 (`0008:06:00.0`, NUMA 0) + any 1 RDMA VF from NUMA 0. NCCL enables
GDR via C2C with `NCCL_NET_GDR_C2C=1`. Transport: `NET/IB/GDRDMA(PCI)`.

### `2nic-aligned` — 1 GPU + 2 NICs, same NUMA

gpu-0 + both RDMA VFs from NUMA 0 (rdma_vf_rail0 + rdma_vf_rail1).
Saturates NUMA 0 — this is the per-GPU maximum on GB300.4.

### `4gpu-4nic` — 4 GPUs + 4 NICs, both NUMA nodes

All 4 GPUs + all 4 RDMA VFs. Used with a `ComputeDomain` channel claim
(see `compute-domain.yaml`) to enable `NCCL_MNNVL_ENABLE=1`.

### `1nic-unaligned` — 1 GPU + 1 NIC, cross-NUMA

gpu-0 (NUMA 0) + any 1 RDMA VF from NUMA 1. NCCL disables GDR; on Grace
the C2C cross-socket interconnect makes the staging cost small (see
results below).

## Running the full test suite

Each test requires deleting the previous MPIJob since the resource claims
are immutable.

```bash
# --- Test 1: 1nic-aligned ---
kubectl apply -f resource-claim-template.yaml
kubectl apply -f mpi-job.yaml   # resourceClaimTemplateName: 1nic-aligned
# Wait for results ...
kubectl delete mpijob nccl-test-dra

# --- Test 2: 2nic-aligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 2nic-aligned
kubectl apply -f mpi-job.yaml
kubectl delete mpijob nccl-test-dra

# --- Test 3: 1nic-unaligned ---
# Edit mpi-job.yaml: resourceClaimTemplateName: 1nic-unaligned
kubectl apply -f mpi-job.yaml
kubectl delete mpijob nccl-test-dra
```

## Orphaned RDMA NICs

The GB200 README previously documented a workaround for RDMA NICs left
inside pod namespaces after pod deletion (see
[issue #137](https://github.com/kubernetes-sigs/dranet/issues/137) and
[containerd/nri#286](https://github.com/containerd/nri/pull/286)).
PR [#180](https://github.com/kubernetes-sigs/dranet/pull/180) (commit
`efba604`) fixes this for `rdma netns=exclusive` mode by returning the RDMA
device to the host namespace **before** the netdev and requesting an
inventory rescan when the netdev path does not fire `NEWLINK`.

This example validates the fix: 3 stress cycles of create/delete on the
2-worker job and one 16-worker multinode job all returned every VF to the
host `ResourceSlice` within 1–4 seconds of pod deletion. No PCI rebind was
necessary.

```bash
# Verify VFs return to the slice on every node after a job is deleted.
kubectl delete mpijob nccl-test-dra
sleep 5
kubectl get resourceslice -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
for rs in data['items']:
    if rs['spec'].get('driver') != 'dra.net': continue
    nics = set()
    for d in rs['spec'].get('devices', []):
        a = d.get('attributes', {})
        if a.get('dra.net/rdma',{}).get('bool') and a.get('dra.net/sriov',{}).get('bool') == False:
            nics.add(a.get('dra.net/ifName',{}).get('string',''))
    if 'rdma_vf_rail0' in nics or 'rdma_vf_rail1' in nics:
        missing = {'rdma_vf_rail0','rdma_vf_rail1','rdma_vf_rail2','rdma_vf_rail3'} - nics
        if missing:
            print(f'{rs[\"spec\"][\"nodeName\"]}: missing {missing}')
"
# (no output → no orphans)
```

## Benchmark Results

### 2-node `all_reduce_perf` (`-b 512M -e 8G -f 2 -g 1`)

1 GPU per worker. Transport: `NET/IB/GDRDMA(PCI)` for NUMA-aligned,
`NET/IB` (no GDR) for cross-NUMA. Settings: `NCCL_MIN_NCHANNELS=8`,
`NCCL_IB_QPS_PER_CONNECTION=2`, `NCCL_IB_DATA_DIRECT=0`.

| Template | GPU | NIC(s) | NUMA relation | Channels | GDR | 8 GB busbw | Avg busbw |
|---|---|---|---|---|---|---|---|
| `1nic-aligned` | gpu-0 (NUMA 0) | 1 RDMA VF (NUMA 0) | same | 8 | yes | 53.90 GB/s | **53.05 GB/s** |
| `2nic-aligned` | gpu-0 (NUMA 0) | 2 RDMA VFs (NUMA 0) | same | 8 | yes | 104.65 GB/s | **99.22 GB/s** |
| `1nic-unaligned` | gpu-0 (NUMA 0) | 1 RDMA VF (NUMA 1) | cross | 8 | no | 53.62 GB/s | **51.42 GB/s** |

### 16-node `all_reduce_perf`, 4 GPUs per rank (`-b 8 -e 32G -f 2 -g 1`)

16 workers × 4 GPUs = 64 ranks. DRA: `4gpu-4nic` + `ComputeDomain` channel
claim. `NCCL_MNNVL_ENABLE=1`, `NCCL_CUMEM_ENABLE=1`, `NCCL_NET_PLUGIN=none`.

| Size | algbw (GB/s) | busbw (GB/s) |
|---|---|---|
| 512 MB | 274.19 | 539.81 |
| 1 GB | 323.79 | 637.47 |
| 2 GB | 366.20 | 720.95 |
| 4 GB | 397.64 | 782.86 |
| 8 GB | 416.97 | 820.91 |
| 16 GB | 423.97 | **834.68** |
| 32 GB | 424.94 | **836.60** |

~837 GB/s sustained busbw at 16 nodes. With only 4 × 400 Gb/s NICs per node
(half of GB200's count), NCCL routes most traffic over the NVLink fabric
(MNNVL) using RoCEv2 only where the NVLink topology does not provide a
direct path — same architectural pattern as GB200.

### Key observations

**NUMA alignment is not penalized on GB300 the way it was on GB200.**
On GB200, cross-NUMA NIC placement dropped throughput from ~46 GB/s to
~25 GB/s (GDR disabled + NCCL reduced channels from 8 to 2). On GB300.4,
the cross-NUMA case still loses `GDRDMA`, but the Grace C2C cross-socket
interconnect lets host-staged transfers saturate the NIC at ~51 GB/s with
the full 8 channels intact. The practical penalty is small.

**Per-NIC throughput exceeds GB200.**
1nic-aligned: ~53 GB/s on GB300 vs ~46 GB/s on GB200 — ConnectX-8 with
the GB300 PCIe path runs the 400 Gb/s NIC closer to line rate. 2nic-aligned
scales near-linearly to 99 GB/s.

**Multinode busbw is fabric-dominated.**
The 16-node 4gpu-4nic test hits ~837 GB/s. The 4 × 400 Gb/s NICs total
200 GB/s of IB bandwidth — busbw exceeds this 4× because NCCL routes
intra-clique traffic over the NVLink fabric (MNNVL via IMEX channels) and
falls back to RoCEv2 only where NVLink does not connect.

**No orphaned NICs observed.**
With `efba604` applied, all VFs returned to the host `ResourceSlice`
within 1–4 seconds of pod deletion. Verified across three 2-node
create/delete cycles and one 16-node multinode run.
