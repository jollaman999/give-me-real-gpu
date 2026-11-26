[English](README.md) | **한국어**

# give-me-real-gpu

실제 GPU 서버 없이 GPU 모니터링 화면을 채워 주는 에뮬레이터입니다.

OpenStack 을 연동하는 관리 플랫폼과 GPU 모니터링 서버를 함께 개발하거나 테스트할 때,
실제 GPU 노드와 수집 에이전트(telegraf) 대신 이 프로그램 하나를 띄우면 됩니다. 에뮬레이트하는 것은 두 가지입니다.

- **가짜 OpenStack API** - keystone, nova 등이 포트 하나(기본 15000)에서 경로로 나뉘어 응답합니다.
  관리 플랫폼은 이를 실제 OpenStack 으로 보고 hypervisor, VM, flavor 를 동기화합니다.
- **메트릭 쓰기** - 같은 토폴로지로 만든 GPU 메트릭(`dcgm`, `gpu_vm_mapping`)과 시스템 메트릭(cpu, mem, disk, net 등)을
  수집 에이전트가 쓰는 것과 같은 모양의 line protocol 로 InfluxDB v1 에 주기적으로(기본 10초마다)
  씁니다.

OpenStack API 의 hypervisor hostname·서버 id 와 InfluxDB 의 `host`·`uuid` 태그가 같은 토폴로지에서 나오므로,
모니터링 서버가 둘을 조인할 수 있습니다. id 는 이름에서 결정적으로 만들어지므로 재시작해도 바뀌지 않습니다.

## 에뮬레이트하는 클라우드 (기본 토폴로지)

| 노드 | GPU 공유 방식 | GPU | VM |
|------|---------------|-----|-----|
| emu-vgpu-01 | VGPU | NVIDIA A10 x4, A10-4Q (GPU 당 최대 6개) | 6, 4, 3, 0 (마지막 GPU 는 미할당) |
| emu-vgpu-02 | VGPU | Tesla T4 x2, T4-4Q (GPU 당 최대 4개) | 4, 2 |
| emu-mig-01 | MIG | A100-SXM4-80GB x4 | GPU0 1g.10gb x7, GPU1 2g.20gb x3, GPU2 3g.40gb x2, GPU3 7g.80gb x1 (일부 인스턴스는 미할당) |
| emu-mig-02 | MIG | H100 80GB HBM3 x2 | 1g.10gb x7, 3g.40gb x2 |
| emu-pt-01 | PASSTHROUGH | L40S x4 | 3 (GPU 하나는 미할당) |
| emu-pt-02 | PASSTHROUGH | A100-PCIE-40GB x4 | 4 |

passthrough 에서는 GPU 하나를 VM 하나만 쓸 수 있습니다(물리적 제약). 모든 이름에 `emu-` 접두사를 붙여 실제 노드와 구분됩니다.

기본 토폴로지 전체(id, IP, flavor 포함)를 보려면 다음을 실행합니다.

```sh
go run ./cmd/give-me-real-gpu -dump-topology
```

이 출력을 고쳐 파일로 저장하고 설정의 `topology_file` 에 지정하면 다른 구성을 에뮬레이트할 수 있습니다.

## 빠른 시작

1. 설정 파일을 만듭니다.

   ```sh
   cp config.example.json config.json
   chmod 644 config.json
   ```

   - `advertise_host` 에는 관리 플랫폼이 접근할 수 있는 이 머신의 주소를 넣습니다(예시의 `192.0.2.10` 은 문서용 주소입니다).
   - `openstack.password` 를 바꿉니다. 이 값은 관리 플랫폼에 credential 을 등록할 때 입력합니다.
   - sink 는 일단 `enabled: false` 로 둡니다. 이 상태에서는 가짜 OpenStack API 만 동작하고 InfluxDB 에는 아무것도 쓰지 않습니다.

   컨테이너는 nonroot(uid 65532)로 실행되므로 권한이 600 인 `config.json` 은 읽지 못합니다(`permission denied`).
   644 파일에 비밀번호를 두고 싶지 않다면 비밀값은 비워 두고 `emulator.env` 에 환경 변수로 넣습니다([환경 변수](#환경-변수) 참고).

2. 실행합니다.

   ```sh
   docker compose up -d --build     # or: make up
   docker compose logs emulator
   ```

3. 시작 로그에서 등록할 값을 확인합니다.

   ```
   level=INFO msg="OpenStack credential to register in the management platform (password is openstack.password from the config)" url=http://192.0.2.10:15000/v3 user=emu-admin project_id=... project_name=emu-gpu domain_id=default region=RegionOne
   ```

4. 관리 플랫폼에 OpenStack credential 을 등록합니다. 인증 URL 에는 로그의 `url`, 사용자에는 `user`,
   비밀번호에는 `openstack.password`, 프로젝트와 도메인에는 로그의 `project_id` 와 `domain_id` 를 씁니다.
   관리 플랫폼이 동기화를 마치면 위 표의 hypervisor 와 VM 이 보여야 합니다.

5. 관리 플랫폼이 돌려준 credential id 를 sink 의 `credential_id` 에 넣고, InfluxDB 계정
   (`username`, `password`)을 채운 뒤 `enabled: true` 로 바꿉니다. sink 로는 모니터링 서버가 읽는 InfluxDB 를 씁니다.

6. 다시 시작합니다.

   ```sh
   docker compose up -d --force-recreate emulator
   docker compose logs -f emulator    # "InfluxDB write" sink=... lines=... appears every interval
   ```

실제로 쓰기 전에 무엇이 나가는지 보려면 `-once` 를 씁니다. 메트릭을 한 번 만들어 stdout 으로 출력합니다(InfluxDB 에는 쓰지 않습니다).
`dry_run: true` 로 두면 루프는 계속 돌고 줄 수만 로그로 남깁니다.

```sh
docker compose run --rm -T emulator -once | head
```

헬스 체크: `curl http://<host>:15000/healthz` 는 `ok` 를 돌려줍니다.

## 로컬 테스트

실제 InfluxDB 에 쓰기 전에 로컬 InfluxDB(1.11.8)를 상대로 처음부터 끝까지 돌려 볼 수 있습니다.

```sh
docker compose --profile local up -d --build
```

sink 는 compose 네트워크 안의 서비스 이름을 가리키게 합니다.

```json
{"name": "local", "url": "http://influxdb:8086", "db": "telegraf", "credential_id": "<any UUID>", "enabled": true}
```

호스트에서는 18086 포트로 조회합니다.

```sh
curl -sG http://localhost:18086/query --data-urlencode db=telegraf --data-urlencode 'q=SHOW MEASUREMENTS'
```

## 설정

`config.json` (`config.example.json` 을 복사해 만듭니다. `config.json` 은 git 에 커밋하지 않습니다).

| 키 | 기본값 | 의미 |
|-----|--------|------|
| `listen` | `:15000` | 가짜 OpenStack API 의 주소. 포트는 catalog URL 에도 들어갑니다 |
| `advertise_host` | (필수) | keystone catalog URL 의 호스트. 관리 플랫폼이 접근할 수 있는 주소 |
| `openstack.password` | (필수) | 가짜 keystone 비밀번호. 사용자와 프로젝트 이름은 토폴로지에서 가져옵니다 |
| `openstack.token_ttl` | `1h` | 발급한 토큰의 유효 기간 |
| `topology_file` | `""` | 토폴로지 JSON 경로. 비우면 기본 6노드 |
| `interval` | `10s` | 메트릭을 만들어 쓰는 주기. 최소 1s |
| `seed` | `1` | 메트릭 값의 난수 시드. sink 마다 `seed + sinks 안의 인덱스` 를 씁니다 |
| `xid_rate` | `0.002` | 한 주기에 VM 하나가 XID 이벤트를 낼 확률 (0~1) |
| `dry_run` | `false` | true 면 InfluxDB 에 쓰지 않고 줄 수만 로그로 남깁니다 |
| `sinks[].name` | (필수) | 로그와 환경 변수 이름에 쓰는 이름 |
| `sinks[].url` | | InfluxDB v1 주소 (`http://host:8086`) |
| `sinks[].db` | | 데이터베이스 이름 |
| `sinks[].username`, `password` | `""` | InfluxDB 계정. 둘 다 비어 있으면 인증 없이 씁니다 |
| `sinks[].credential_id` | `""` | 관리 플랫폼이 돌려준 credential id(UUID). `credential_id` 태그 값과 `uuid` 태그의 접두사가 됩니다 |
| `sinks[].enabled` | `false` | 켜진 sink 에만 씁니다. 켜진 sink 에는 url, db, credential_id 가 있어야 합니다 |

기간 값은 `"10s"`, `"1h"` 같은 문자열입니다. 알 수 없는 키는 오타로 보고 시작을 거부합니다.

### 환경 변수

비어 있지 않은 환경 변수가 설정 파일보다 우선합니다. repo 루트에 `emulator.env` 가 있으면 compose 가 넘겨 줍니다(git 에 커밋하지 않습니다).

| 변수 | 덮어쓰는 값 |
|------|-------------|
| `EMU_OS_PASSWORD` | `openstack.password` |
| `EMU_ADVERTISE_HOST` | `advertise_host` |
| `EMU_LISTEN` | `listen` |
| `EMU_SINK_<NAME>_USERNAME` | `sinks[].username` |
| `EMU_SINK_<NAME>_PASSWORD` | `sinks[].password` |
| `EMU_SINK_<NAME>_CREDENTIAL_ID` | `sinks[].credential_id` |
| `EMU_SINK_<NAME>_ENABLED` | `sinks[].enabled` (`true`/`false`) |

`<NAME>` 은 sink 이름을 대문자로 바꾸고 영숫자가 아닌 문자를 모두 `_` 로 바꾼 것입니다. 예를 들어 `site-a` 는 `EMU_SINK_SITE_A_PASSWORD` 가 됩니다.

```sh
# emulator.env (fill in the values after "=")
EMU_OS_PASSWORD=
EMU_SINK_SITE_A_PASSWORD=
```

## 명령줄

| 플래그 | 의미 |
|--------|------|
| `-config <path>` | 설정 파일. 기본값 `config.json` (컨테이너 안에서는 작업 디렉터리가 `/etc/give-me-real-gpu`) |
| `-dump-topology` | 기본 토폴로지를 JSON 으로 출력하고 끝납니다. 설정 파일이 필요 없습니다 |
| `-once` | 켜진 sink 마다 메트릭을 한 번 만들어 stdout 으로 출력하고 끝납니다. 아무것도 쓰지 않습니다 |
| `-version` | 버전을 출력합니다 |

## 빌드

```sh
make build     # bin/give-me-real-gpu
make test
make vet       # go vet + gofmt check
make docker    # image give-me-real-gpu:<git describe>
make up / make down
```

Go 표준 라이브러리만 씁니다. 이미지는 `golang:1.27-alpine` 에서 빌드하고 `gcr.io/distroless/static-debian12:nonroot` 에서 실행합니다.

## 주의할 점

- **켜진 sink 는 실제 InfluxDB 에 씁니다.** 실제 노드의 메트릭도 받는 InfluxDB 에 붙일 때는
  관리 플랫폼에 이 에뮬레이터용 credential 을 따로 등록하고 그 credential id 만 씁니다. 메트릭은
  credential id 로 구분하므로 실제 노드의 credential id 를 다시 쓰면 데이터가 섞입니다.
- 이 프로그램은 자신이 쓴 행을 지우지 않습니다. 테스트가 끝나면 sink 를 끄고, 필요하면 InfluxDB 쪽에서 credential id 기준으로 지웁니다.
- 토폴로지(노드나 VM 이름)를 바꾸면 id 도 바뀝니다. 예전 hypervisor 와 VM 이 관리 플랫폼에 남아 있을 수 있습니다.
- 가짜 API 는 관리 플랫폼이 동기화에 쓰는 조회 호출만 에뮬레이트합니다. VM 생성·삭제 같은 쓰기 요청은 처리하지 않습니다.

## 라이선스

MIT. `LICENSE` 를 참고하세요.
