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

Edit that output, save it as a file and set `topology_file` in the config to emulate a different layout.

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

2. Start it.

   ```sh
   docker compose up -d --build     # or: make up
   docker compose logs emulator
   ```

3. Find the values to register in the startup log.

   ```
   level=INFO msg="OpenStack credential to register in the management platform (password is openstack.password from the config)" url=http://192.0.2.10:15000/v3 user=emu-admin project_id=... project_name=emu-gpu domain_id=default region=RegionOne
   ```

4. Register an OpenStack credential in the management platform. Use `url` from the log as the auth URL, `user` as the user,
   `openstack.password` as the password, and `project_id` and `domain_id` from the log for the project and domain.
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
- Changing the topology (node or VM names) changes the ids. Old hypervisors and VMs may remain in the management platform.
- The fake API only emulates the read calls the management platform uses for syncing. Write requests such as creating or deleting VMs are not handled.

## License

MIT. See `LICENSE`.
