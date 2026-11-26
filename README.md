**English** | [한국어](README.ko.md)

# give-me-real-gpu

An emulator that fills GPU monitoring screens without real GPU servers.

When you develop or test a management platform that integrates with OpenStack, together with a GPU monitoring server,
run this single program instead of real GPU nodes and collection agents (telegraf). It emulates two things.

- **A fake OpenStack API** - keystone, nova and friends answer on one port (default 15000), split by path.
  The management platform treats it as a real OpenStack and syncs hypervisors, VMs and flavors from it.
- **Metric writes** - GPU metrics (`dcgm`, `gpu_vm_mapping`) and system metrics (cpu, mem, disk, net, ...) built from the
  same topology are written to InfluxDB v1 periodically (default every 10 seconds), as line protocol shaped like what the
  collection agent writes.

The hypervisor hostnames and server ids of the OpenStack API and the `host` and `uuid` tags in InfluxDB come from the same
topology, so the monitoring server can join the two. Ids are derived deterministically from names and stay the same across restarts.

## The emulated cloud (default topology)

| Node | GPU sharing mode | GPUs | VMs |
|------|------------------|------|-----|
| emu-vgpu-01 | VGPU | NVIDIA A10 x4, A10-4Q (up to 6 per GPU) | 6, 4, 3, 0 (last GPU unassigned) |
| emu-vgpu-02 | VGPU | Tesla T4 x2, T4-4Q (up to 4 per GPU) | 4, 2 |
| emu-mig-01 | MIG | A100-SXM4-80GB x4 | GPU0 1g.10gb x7, GPU1 2g.20gb x3, GPU2 3g.40gb x2, GPU3 7g.80gb x1 (some instances unassigned) |
| emu-mig-02 | MIG | H100 80GB HBM3 x2 | 1g.10gb x7, 3g.40gb x2 |
| emu-pt-01 | PASSTHROUGH | L40S x4 | 3 (one GPU unassigned) |
| emu-pt-02 | PASSTHROUGH | A100-PCIE-40GB x4 | 4 |

With passthrough only one VM can use a GPU (a physical constraint). Every name has the `emu-` prefix so it stands apart from real nodes.

To see the full default topology (ids, IPs and flavors included):

```sh
go run ./cmd/give-me-real-gpu -dump-topology
```

To emulate a different layout, write your own topology file (see [Custom topology](#custom-topology)).

## Metrics written

Every collection (every `interval`) produces one batch of line protocol per enabled sink. Each hypervisor and each VM
in the topology acts as one collection agent and puts these global tags on its rows.

| Tag | Hypervisor agent | VM agent |
|-----|------------------|----------|
| `uuid` | `<credential_id>:<hostname>` | `<credential_id>:<server id>` |
| `credential_id` | `credential_id` of the sink | `credential_id` of the sink |
| `cloud_service` | `openstack` | `openstack` |
| `type` | `host` | `vm` |
| `gpu_type` | `BARE_METAL` on VGPU nodes, `MIG` on MIG nodes, `NONE` on passthrough nodes | `VGPU`, `MIG` or `PASSTHROUGH` (mode of its hypervisor) |
| `collection_type` | `agent` | `agent` |
| `host` | hypervisor hostname | VM name |
| `time_diff` | `0` | `0` or `-1`, fixed per VM. The timestamps of that VM are shifted by the same number of seconds |
| `project_id` | not set | project id of the topology |

The hypervisor `gpu_type` follows how the monitoring server detects it from `nvidia-smi -q -x`: a vGPU host driver reports
`Host VGPU`, which falls back to `BARE_METAL`, and a passthrough host has no nvidia driver, which gives `NONE`.

Field types are `i` (integer), `f` (float) and `s` (string). "every" is every collection; 60s and 5m rows are written on
the first collection of each new 60 second or 5 minute slot (the first collection after start includes both).

| Measurement (kind) | Written by | Interval | Tags besides the global ones | Fields |
|--------------------|------------|----------|------------------------------|--------|
| `dcgm` physical GPU | VGPU and MIG hypervisors, one per GPU (passthrough hypervisors write none) | every | `index`, `gpu`, `pstate` (`P0`/`P8`), `name`, `modelName`, `arch`, `UUID` (`GPU-...`), `compute_mode`, `pci_bus_id`, `DCGM_FI_DEV_PCI_BUSID`, `DCGM_FI_DRIVER_VERSION`, `DCGM_FI_DEV_VBIOS_VERSION`, `DCGM_FI_NVML_VERSION` | `gpu_temp`, `power_usage`, `fb_total`, `fb_used`, `fb_free`, `sm_clock`, `mem_clock`, `clocks_event_reasons`, `pcie_replay_counter`, `ecc_sbe_vol_total`, `ecc_dbe_vol_total`, `ecc_sbe_agg_total`, `ecc_dbe_agg_total` (f), `pstate` (i). Not on MIG: `gpu_util`, `mem_copy_util`, `enc_util`, `dec_util` (f). HBM boards (A100, H100, H200): `memory_temp` (f). Turing, Volta, Pascal: `retired_pages_dbe`, `retired_pages_pending` (f); other architectures: `correctable_remapped_rows`, `uncorrectable_remapped_rows`, `row_remap_failure`, `row_remap_pending` (f) |
| `dcgm` GPM, device | MIG hypervisors, one per GPU | every | same as the physical GPU row | `gpu_util`, `sm_occupancy`, `graphics_util`, `dram_bw_util`, `fp32_util`, `fp16_util`, `fp64_util` (f), `server_time` (i) |
| `dcgm` GPM, MIG instance | MIG hypervisors, one per assigned instance | every | same as the vGPU/MIG VM row below (`type=vm`) | same as the GPM device row |
| `dcgm` vGPU/MIG VM | VGPU and MIG hypervisors, one per VM (written by the mapping script on the hypervisor) | 60s | its own set instead of the global tags: `uuid=<credential_id>:<server id>`, `type=vm`, `host=<VM name>`, `gpu_type` (`VGPU` or `MIG`), `credential_id`, `cloud_service`, `collection_type`, `time_diff=0`, `UUID` (vGPU uuid), `gpu` (position of the vGPU on its GPU), `modelName` (vGPU type name), `gpu_instance_id` (`N/A` on VGPU), `physical_gpu_uuid`, `pci_bus_id`, `DCGM_FI_DEV_PCI_BUSID`, `DCGM_FI_DRIVER_VERSION`, `DCGM_FI_DEV_VBIOS_VERSION`. No `project_id` | `fb_total`, `fb_used`, `fb_free`, `vgpu_license_status`, `mig_mode` (f), `pstate`, `server_time` (i). VGPU only: `gpu_util`, `mem_copy_util`, `enc_util`, `dec_util` (f) |
| `dcgm` passthrough guest | passthrough VMs | every | the physical GPU tags as the guest sees them (`index=0`, guest PCI address, guest driver) plus `DCGM_FI_CUDA_DRIVER_VERSION` | same as the physical GPU row, with the utilization fields |
| `dcgm` cuda_probe | VGPU and MIG VMs | 5m | `UUID` (vGPU uuid without `GPU-`), `DCGM_FI_CUDA_DRIVER_VERSION` | `cuda_probe` (i, always 1), `server_time` (i) |
| `dcgm` XID | a passthrough VM (`uuid` of the VM), or the hypervisor of a VGPU/MIG VM (`uuid` of the hypervisor) | probability `xid_rate` per GPU VM per collection | no global tags, only `err_code`, `DCGM_FI_DEV_PCI_BUSID` (`0000:bb:dd`), `uuid`, `UUID`, `modelName`, `DCGM_FI_DRIVER_VERSION` | `xid_errors` (f, the XID code), `err_msg` (s, the kernel `NVRM: Xid` line) |
| `gpu_vm_mapping` vGPU/MIG assigned | VGPU and MIG hypervisors, one per VM | 60s | `openstack_vm_name`, `openstack_vm_uuid`, `host_bdf`, `guest_bdf=vgpu`, `vendor_id=10de`, `device_id`, `status=assigned`, `driver=nvidia`, `gpu_type` (`vgpu` or `mig`, replaces the global value), `vgpu_uuid` | `mapped` (i, 1), `vgpu_fb_mb`, `vgpu_max_instances`, `vgpu_active_instances`, `server_time` (i), `vgpu_profile`, `vgpu_model_name`, `gpu_uuid` (s) |
| `gpu_vm_mapping` passthrough assigned | passthrough hypervisors, one per assigned GPU | 60s | as above without `vgpu_uuid`; `guest_bdf` is the guest PCI address, `driver=vfio-pci`, `gpu_type=passthrough` | `mapped` (i, 1), `server_time` (i) |
| `gpu_vm_mapping` unassigned | any hypervisor, one per GPU with no VM | 60s | `openstack_vm_name=N/A`, `openstack_vm_uuid=N/A`, `guest_bdf=N/A`, `host_bdf`, `vendor_id`, `device_id`, `status=unassigned`, `driver` (`nvidia` or `vfio-pci`), `gpu_type` (lower case mode) | `mapped` (i, 0), `vgpu_active_instances` (i, 0), `server_time` (i), `gpu_uuid` (s, not on passthrough) |
| `cpu` | all agents, one per cpu plus `cpu-total` | every | `cpu` (`cpu0`, `cpu1`, ..., `cpu-total`) | `usage_user`, `usage_system`, `usage_idle`, `usage_nice`, `usage_iowait`, `usage_irq`, `usage_softirq`, `usage_steal` (f), `server_time` (i) |
| `mem` | all agents | every | - | `total`, `used`, `free`, `cached`, `buffered`, `available`, `active`, `inactive`, `shared` (i), `used_percent`, `available_percent` (f), `server_time` (i) |
| `swap` | all agents (`total` is 0 on VMs) | every | - | `total`, `used`, `free` (i), `used_percent` (f), `server_time` (i) |
| `disk` | all agents, one per mount (hypervisor `/` and `/boot`, VM `/`) | every | `device`, `fstype`, `mode`, `path`, `label` (VM only) | `total`, `used`, `free`, `inodes_total`, `inodes_used`, `inodes_free` (i), `used_percent`, `inodes_used_percent` (f), `server_time` (i) |
| `diskio` | all agents, one disk each | every | `name`, `wwid` (hypervisor only) | `reads`, `writes`, `read_bytes`, `write_bytes`, `read_time`, `write_time`, `io_time`, `iops_in_progress` (i), `io_await`, `io_svctm`, `io_util` (f), `server_time` (i) |
| `net` interface | all agents (hypervisor `eno1`, `eno2`; VM `ens3`) | every | `interface` | `bytes_recv`, `bytes_sent`, `packets_recv`, `packets_sent`, `err_in`, `err_out`, `drop_in`, `drop_out`, `server_time` (i) |
| `net` ip | hypervisors with a `host_ip` | 60s | - | `ip` (s, `host_ip` of the topology), `server_time` (i) |
| `system` | all agents | every | - | `load1`, `load5`, `load15` (f), `n_cpus`, `n_physical_cpus`, `uptime`, `server_time` (i). VM only: `n_users`, `n_unique_users` (i) |
| `processes` | all agents | every | - | `blocked`, `dead`, `idle`, `running`, `sleeping`, `stopped`, `total`, `unknown`, `zombies`, `server_time` (i) |
| `procstat` | all agents, 5 processes each | 60s | `process_name`, `user`, `filter=exclude_empty_cmdline` | `cpu_usage`, `memory_usage` (f), `server_time` (i) |

With the default topology (6 hypervisors and 45 VMs, so 51 agents) one collection is 2032 lines, 2378 on a 60 second
boundary and 2416 on a 5 minute boundary, plus any XID rows. About 1600 of them are `cpu` rows.
Values are seeded random walks (each sink uses `seed + its index`, each entity has its own random source), GPU load
drives temperature, power, clocks and the VM cpu load, and cumulative counters (`diskio`, `net` bytes and packets, `uptime`)
only increase. An XID row appears with probability `xid_rate` per GPU VM per collection, with one of the codes 13, 31, 43, 48, 63, 79, 94 or 119.

Some quirks match the real collection pipeline on purpose.

- Physical GPU rows and passthrough guest `dcgm` rows have no `server_time`, because the agent's nvidia-smi to dcgm processor drops the original fields.
- XID rows have none of the global tags and no `server_time` or `time_diff`, because the agent's xid processor builds a new metric.
- A MIG GPU with at least one assigned instance counts as assigned as a whole, so its unused instances get no `unassigned` row.

## Quick start

1. Create the config file.

   ```sh
   cp config.example.json config.json
   chmod 644 config.json
   ```

   - Set `advertise_host` to the address of this machine that the management platform can reach (`192.0.2.10` in the example is a documentation address).
   - Change `openstack.password`. You enter this value when registering the credential in the management platform.
   - Leave the sinks at `enabled: false` for now. In this state only the fake OpenStack API runs and nothing is written to InfluxDB.

   The container runs as nonroot (uid 65532), so it cannot read a `config.json` with mode 600 (`permission denied`).
   If you do not want passwords in a 644 file, leave the secrets empty and put them in `emulator.env` as environment variables (see [Environment variables](#environment-variables)).
   With compose, create `emulator.env` in the repo root and fill in the password after `=`. It overrides `openstack.password`.

   ```sh
   # emulator.env (fill in the value after "=")
   EMU_OS_PASSWORD=
   ```

2. Start it.

   ```sh
   docker compose up -d --build     # or: make up
   docker compose logs emulator
   ```

3. Find the values to register in the startup log.

   ```
   level=INFO msg="OpenStack credential to register in the management platform (password is openstack.password from the config)" url=http://192.0.2.10:15000/v3 user=emu-admin project_id=f2aeea254abf2861de80ddccb3cc7225 project_name=emu-gpu domain_id=default region=RegionOne
   ```

4. Register an OpenStack credential in the management platform with these values.

   | Field | Value |
   |-------|-------|
   | Auth URL | `http://<advertise_host>:<listen port>/v3` (for example `http://192.0.2.10:15000/v3`) |
   | Domain ID | `default` |
   | User | `emu-admin` |
   | Password | `openstack.password`, or `EMU_OS_PASSWORD` when it is set (with compose, the value after `EMU_OS_PASSWORD=` in `emulator.env`) |
   | Project ID | `f2aeea254abf2861de80ddccb3cc7225` |
   | Project name | `emu-gpu` |
   | Region | `RegionOne` |

   Domain, user, project and region are fixed values of the default topology (the project id is derived from the project name),
   so they stay the same across restarts. With `topology_file` they are whatever that file says; read them from the startup log.
   Once the management platform syncs, the hypervisors and VMs in the table above should appear.

5. Put the credential id returned by the management platform into the sink's `credential_id`, fill in the InfluxDB account
   (`username`, `password`) and set `enabled: true`. Use the InfluxDB that the monitoring server reads as the sink.

6. Restart.

   ```sh
   docker compose up -d --force-recreate emulator
   docker compose logs -f emulator    # "InfluxDB write" sink=... lines=... appears every interval
   ```

To see what would be sent before writing anything, use `-once`: it generates metrics once and prints them to stdout (nothing is written to InfluxDB).
With `dry_run: true` the loop keeps running and only logs line counts.

```sh
docker compose run --rm -T emulator -once | head
```

Health check: `curl http://<host>:15000/healthz` returns `ok`.

## Local testing

Before writing to a real InfluxDB you can run end to end against a local InfluxDB (1.11.8).

```sh
docker compose --profile local up -d --build
```

Point the sink at the service name inside the compose network.

```json
{"name": "local", "url": "http://influxdb:8086", "db": "telegraf", "credential_id": "<any UUID>", "enabled": true}
```

From the host, read it on port 18086.

```sh
curl -sG http://localhost:18086/query --data-urlencode db=telegraf --data-urlencode 'q=SHOW MEASUREMENTS'
```

## Custom topology

The default six nodes are built in code. To emulate a different cloud, describe it in a JSON file and set `topology_file`.
A topology file is used as written: the emulator does not generate ids, addresses or names for it.

1. Start from the default topology.

   ```sh
   go run ./cmd/give-me-real-gpu -dump-topology > topology.json
   # without Go, from the image that docker compose built
   docker run --rm give-me-real-gpu:dev -dump-topology > topology.json
   ```

2. Edit it. Delete the nodes, GPUs and VMs you do not need, or copy one and give the copy new names and ids
   (`uuidgen` gives a new UUID). Keep the [rules](#rules-checked-at-startup) below.

3. Mount the file into the container. compose only mounts `config.json`, so add a second bind mount to `compose.yaml`.
   Without it the program stops with `open topology file: ... no such file or directory`.

   ```yaml
   services:
     emulator:
       volumes:
         - type: bind
           source: ./config.json
           target: /etc/give-me-real-gpu/config.json
           read_only: true
           bind:
             create_host_path: false
         - type: bind
           source: ./topology.json
           target: /etc/give-me-real-gpu/topology.json
           read_only: true
           bind:
             create_host_path: false
   ```

   Then point the config at the path inside the container, and make the file readable for the nonroot user (uid 65532).

   ```json
   "topology_file": "/etc/give-me-real-gpu/topology.json"
   ```

   ```sh
   chmod 644 topology.json
   ```

   Without a container, `topology_file` is a normal path. A relative path is resolved from the working directory, not from the config file.

4. Restart and check the counts in the first log line.

   ```sh
   docker compose up -d --force-recreate emulator
   docker compose logs emulator | head -2
   # level=INFO msg="give-me-real-gpu starting" ... hypervisors=1 vms=1
   ```

   A file that breaks a rule stops the program, and the error lists every problem it found, for example:

   ```
   level=ERROR msg="failed to load the topology" err="validate topology file /etc/give-me-real-gpu/topology.json: hypervisor emu-pt-01 gpu 0: passthrough GPU has 2 VMs (max 1)\nvm emu-pt01-g1-1: gpu_index 1 does not match the GPU it is attached to, 0 ..."
   ```

### A minimal topology

One vGPU node with one A10 and one VM. Every value is copied from the default topology.

```json
{
  "region": "RegionOne",
  "domain_id": "default",
  "project_id": "f2aeea254abf2861de80ddccb3cc7225",
  "project_name": "emu-gpu",
  "user_id": "bbd08f19db80d3d802e4789fc042589f",
  "user_name": "emu-admin",
  "hypervisors": [
    {
      "id": 1,
      "uuid": "822a5a71-e486-59c6-b836-7f4fae56f5a3",
      "hostname": "emu-vgpu-01",
      "host_ip": "192.0.2.11",
      "mode": "VGPU",
      "vcpus": 128,
      "memory_mb": 1048576,
      "local_gb": 3576,
      "driver_version": "595.91.04",
      "cuda_version": "13020",
      "gpus": [
        {
          "index": 0,
          "uuid": "GPU-de1a1acc-d068-5846-a4eb-6cf09db837a0",
          "bus_id": "00000000:18:00.0",
          "host_bdf": "18:00.0",
          "model_name": "NVIDIA A10",
          "device_id": "2236",
          "memory_mib": 23028,
          "vbios": "94.02.5C.00.0A",
          "arch": "Ampere",
          "profile": {"name": "NVIDIA A10-4Q", "type": "nvidia-592", "framebuffer_mb": 4096, "max_instances": 6},
          "vms": [
            {
              "id": "da322bfb-45f5-52df-8e74-486cd3903278",
              "name": "emu-vgpu01-g0-1",
              "instance_name": "instance-00000001",
              "flavor_id": "7c97c4d2-368c-51dc-ab66-b69b6c9ed3b1",
              "ip": "198.51.100.11",
              "hypervisor": "emu-vgpu-01",
              "gpu_index": 0,
              "mode": "VGPU",
              "vgpu_uuid": "6d62e585-5e92-5ddf-8911-a4400df629de",
              "mig_gpu_instance_id": -1,
              "guest_bdf": "00:05.0",
              "guest_driver_version": "595.91.07",
              "vcpus": 8,
              "memory_mb": 32768,
              "disk_gb": 100
            }
          ]
        }
      ]
    }
  ],
  "flavors": [
    {
      "id": "7c97c4d2-368c-51dc-ab66-b69b6c9ed3b1",
      "name": "emu-vgpu-a10-4q",
      "vcpus": 8,
      "ram_mb": 32768,
      "disk_gb": 100,
      "extra_specs": {"resources:VGPU": "1", "trait:CUSTOM_NVIDIA_A10_4Q": "required"}
    }
  ]
}
```

A MIG GPU has `mig_instances` instead of `profile`, and each of its VMs names the instance it uses:

```json
"mig_instances": [
  {"gpu_instance_id": 7, "profile": "1g.10gb", "slices": 1, "memory_mib": 9728,
   "vgpu_profile": {"name": "NVIDIA A100D-1-10C", "type": "nvidia-699", "framebuffer_mb": 10240, "max_instances": 7}}
]
```

```json
"mode": "MIG", "vgpu_uuid": "104b9515-0c86-5e9e-8603-f8122f35a1f3", "mig_gpu_instance_id": 7
```

A passthrough GPU has neither. Its VM has no `vgpu_uuid` and `mig_gpu_instance_id` is `-1`.
Passthrough nodes leave `driver_version` and `cuda_version` empty, since the host has no NVIDIA driver.

### Keys

| Where | Key | Meaning |
|-------|-----|---------|
| top | `region`, `domain_id` | Region of the keystone catalog and keystone domain |
| top | `project_id`, `project_name` | Project every VM belongs to. `project_id` is required |
| top | `user_id`, `user_name` | Fake keystone user. `user_name` is required and is the user you register in the management platform |
| `hypervisors[]` | `id`, `uuid` | nova hypervisor integer id and uuid |
| `hypervisors[]` | `hostname` | `hypervisor_hostname` in nova and the `host` tag of host metrics |
| `hypervisors[]` | `host_ip` | nova `host_ip` and the address in net metrics |
| `hypervisors[]` | `mode` | `VGPU`, `MIG` or `PASSTHROUGH`. Also the `gpu_type` tag |
| `hypervisors[]` | `vcpus`, `memory_mb`, `local_gb` | Physical resources reported by nova |
| `hypervisors[]` | `driver_version`, `cuda_version` | Host NVIDIA driver and CUDA driver version (`13020` form). Empty on passthrough nodes |
| `gpus[]` | `index` | nvidia-smi index, from 0 |
| `gpus[]` | `uuid` | `GPU-` followed by a UUID |
| `gpus[]` | `bus_id`, `host_bdf` | PCI address in two forms: `00000000:18:00.0` and `18:00.0` (lower case) |
| `gpus[]` | `model_name`, `device_id`, `memory_mib`, `vbios`, `arch` | Model, PCI device id (vendor is always 10de), framebuffer, VBIOS and architecture |
| `gpus[]` | `profile` | VGPU nodes only: the vGPU type created on this GPU (`name`, mdev `type`, `framebuffer_mb`, `max_instances`) |
| `gpus[]` | `mig_instances` | MIG nodes only: GPU instances (`gpu_instance_id`, `profile`, `slices`, `memory_mib`, `vgpu_profile`) |
| `gpus[]` | `vms` | VMs using this GPU. `[]` means the GPU is unassigned |
| `vms[]` | `id`, `name`, `instance_name` | nova server id, server name (also the `host` tag of VM metrics) and libvirt name |
| `vms[]` | `hypervisor`, `gpu_index`, `mode` | Where the VM runs. Must match the node and GPU it is listed under |
| `vms[]` | `flavor_id` | Id of an entry in `flavors` |
| `vms[]` | `ip` | Fixed ip |
| `vms[]` | `vgpu_uuid` | mdev uuid of the vGPU (VGPU and MIG). Empty for passthrough |
| `vms[]` | `mig_gpu_instance_id` | `gpu_instance_id` of the MIG instance it uses. `-1` on other nodes |
| `vms[]` | `guest_bdf`, `guest_driver_version` | PCI address and NVIDIA driver inside the VM |
| `vms[]` | `vcpus`, `memory_mb`, `disk_gb` | VM size |
| `flavors[]` | `id`, `name`, `vcpus`, `ram_mb`, `disk_gb`, `extra_specs` | nova flavors. GPU flavors carry `resources:VGPU` or `pci_passthrough:alias` in `extra_specs` |

### Rules checked at startup

- `project_id` and `user_name` are set. Unknown keys are treated as typos and rejected.
- These are unique: hypervisor `hostname`, `id` and `uuid`; GPU `uuid`; GPU `index` and `bus_id` within a node;
  VM `name`, `id`, `instance_name` and `vgpu_uuid`; flavor `id` and `name`.
- `mode` is `VGPU`, `MIG` or `PASSTHROUGH`, and each VM has the same `mode` as its node.
- Each VM's `hypervisor` is the `hostname` of the node it is listed under, its `gpu_index` is the `index` of that GPU,
  and its `flavor_id` exists in `flavors`.
- VGPU: every GPU has a `profile`, a GPU has at most `profile.max_instances` VMs, and every VM has a `vgpu_uuid`.
- MIG: the `slices` of a GPU's instances add up to 7 or less, `gpu_instance_id` is unique within the GPU, every VM's
  `mig_gpu_instance_id` names an instance on its GPU, an instance has at most one VM, and every VM has a `vgpu_uuid`.
- PASSTHROUGH: a GPU has at most one VM, and VMs have no `vgpu_uuid`.
- On VGPU and PASSTHROUGH nodes `mig_gpu_instance_id` is `-1`.

Formats are not checked. Keep the forms the dump uses (UUIDs, 32 hex characters for `project_id` and `user_id`),
because the management platform and the monitoring server store these values.

## Settings

`config.json` (copy `config.example.json`; `config.json` is not committed to git).

| Key | Default | Meaning |
|-----|---------|---------|
| `listen` | `:15000` | Address of the fake OpenStack API. The port also goes into the catalog URLs |
| `advertise_host` | (required) | Host of the keystone catalog URLs. An address the management platform can reach |
| `openstack.password` | (required) | Fake keystone password. User and project names come from the topology |
| `openstack.token_ttl` | `1h` | Lifetime of issued tokens |
| `topology_file` | `""` | Path to a topology JSON. Empty means the default 6 nodes |
| `interval` | `10s` | How often metrics are generated and written. At least 1s |
| `seed` | `1` | Random seed for metric values. Each sink uses `seed + its index in sinks` |
| `xid_rate` | `0.002` | Probability that a VM emits an XID event in one interval (0 to 1) |
| `dry_run` | `false` | When true, logs line counts instead of writing to InfluxDB |
| `sinks[].name` | (required) | Name used in logs and environment variable names |
| `sinks[].url` | | InfluxDB v1 address (`http://host:8086`) |
| `sinks[].db` | | Database name |
| `sinks[].username`, `password` | `""` | InfluxDB account. When both are empty, writes are sent without authentication |
| `sinks[].credential_id` | `""` | Credential id (UUID) returned by the management platform. Becomes the `credential_id` tag and the prefix of the `uuid` tag |
| `sinks[].enabled` | `false` | Only enabled sinks are written to. An enabled sink needs url, db and credential_id |

Durations are strings such as `"10s"` or `"1h"`. Unknown keys are treated as typos and the program refuses to start.

### InfluxDB sinks

Each entry in `sinks` is one InfluxDB v1 database. In this example `site-a` writes with an account, and `site-b` is an
InfluxDB with authentication disabled, reached over https and not enabled yet.

```json
"sinks": [
  {
    "name": "site-a",
    "url": "http://influxdb-a.example.com:8086",
    "db": "telegraf",
    "username": "<influxdb-user>",
    "password": "<change-me>",
    "credential_id": "3b9f6a52-8c1d-4e7a-9f20-6d4c5e8b1a07",
    "enabled": true
  },
  {
    "name": "site-b",
    "url": "https://influxdb-b.example.com:8086",
    "db": "telegraf",
    "username": "",
    "password": "",
    "credential_id": "",
    "enabled": false
  }
]
```

| Key | Where the value comes from |
|-----|----------------------------|
| `url` | The InfluxDB the monitoring server reads, where the real collection agents write. `http://` or `https://`, port 8086 by default |
| `db` | The database those agents write to (with telegraf, `database` in `[[outputs.influxdb]]`) |
| `username`, `password` | An account that can write to `db`, such as the agents' account (telegraf `username` and `password`). Leave both empty when authentication is disabled |
| `credential_id` | The id the management platform gives the OpenStack credential you registered in [Quick start](#quick-start) step 4 |

How the emulator writes:

- Every `interval` it sends one batch per enabled sink as `POST <url>/write?db=<db>&precision=ns`, gzip-compressed, up to 5000 lines per request.
- When `username` or `password` is set it uses HTTP Basic authentication.
- It does not create the database. `db` must already exist and the account needs write permission on it.
- A network error or 5xx is retried once after 1 second. A 400 (for example a field type conflict) is logged and the remaining requests are still sent.
- The log shows `msg="InfluxDB write" sink=site-a lines=...` for each successful batch and `msg="InfluxDB write failed"` with the error otherwise.

A disabled sink is not checked, so it can be left half filled in. To keep the account out of `config.json`, leave `username`
and `password` empty there and set them in `emulator.env` (see [Environment variables](#environment-variables)):

```sh
# emulator.env (fill in the values after "=")
EMU_SINK_SITE_A_USERNAME=
EMU_SINK_SITE_A_PASSWORD=
```

### Environment variables

Non-empty environment variables override the config file. compose passes `emulator.env` from the repo root if it exists (it is not committed to git).

| Variable | Overrides |
|----------|-----------|
| `EMU_OS_PASSWORD` | `openstack.password` |
| `EMU_ADVERTISE_HOST` | `advertise_host` |
| `EMU_LISTEN` | `listen` |
| `EMU_SINK_<NAME>_USERNAME` | `sinks[].username` |
| `EMU_SINK_<NAME>_PASSWORD` | `sinks[].password` |
| `EMU_SINK_<NAME>_CREDENTIAL_ID` | `sinks[].credential_id` |
| `EMU_SINK_<NAME>_ENABLED` | `sinks[].enabled` (`true`/`false`) |

`<NAME>` is the sink name upper-cased with every non-alphanumeric character replaced by `_`. For example `site-a` becomes `EMU_SINK_SITE_A_PASSWORD`.

```sh
# emulator.env (fill in the values after "=")
EMU_OS_PASSWORD=
EMU_SINK_SITE_A_PASSWORD=
```

## Command line

| Flag | Meaning |
|------|---------|
| `-config <path>` | Config file. Default `config.json` (inside the container the working directory is `/etc/give-me-real-gpu`) |
| `-dump-topology` | Print the default topology as JSON and exit. No config file needed |
| `-once` | Generate metrics once per enabled sink, print them to stdout and exit. Nothing is written |
| `-version` | Print the version |

## Build

```sh
make build     # bin/give-me-real-gpu
make test
make vet       # go vet + gofmt check
make docker    # image give-me-real-gpu:<git describe>
make up / make down
```

Only the Go standard library is used. The image is built on `golang:1.27-alpine` and runs on `gcr.io/distroless/static-debian12:nonroot`.

## Caveats

- **An enabled sink writes to a real InfluxDB.** When attaching to an InfluxDB that also receives metrics from real nodes,
  register a separate credential for this emulator in the management platform and use only its credential id. Metrics are told
  apart by credential id, so reusing a real node's credential id mixes the data.
- This program never deletes rows it has written. When testing is done, disable the sink and, if needed, delete by credential id on the InfluxDB side.
- When hypervisors or VMs leave the topology or get new ids, the old ones may remain in the management platform. In the default
  topology ids are derived from names, so renaming changes them. In a topology file ids are whatever the file says.
- The fake API only emulates the read calls the management platform uses for syncing. Write requests such as creating or deleting VMs are not handled.

## License

MIT. See `LICENSE`.
