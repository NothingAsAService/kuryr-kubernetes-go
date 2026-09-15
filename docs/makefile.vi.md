# Makefile và quy trình phát triển

Tham chiếu mọi target của [Makefile](../Makefile), cùng những gì cần biết để
chạy được chúng.

`make help` in ra danh sách rút gọn ngay trong terminal.

---

## 1. Bắt đầu nhanh

```sh
make check       # generate + vet + test + lint — đúng những gì CI chạy
make build       # bốn binary vào bin/, cho chính máy này
make run         # chạy controller với ~/.kube/config và etc/kuryr.conf
```

Lần chạy đầu sẽ tải các công cụ (controller-gen, kustomize, setup-envtest,
golangci-lint, kind, helmify) ở phiên bản đã ghim. Xem §6.

---

## 2. Biến cấu hình

Đặt trên dòng lệnh: `make docker-build TAG=v1.2.3`.

| Biến | Mặc định | Ý nghĩa |
| --- | --- | --- |
| `REGISTRY` | `quay.io/kuryr` | Tiền tố registry cho cả hai image |
| `TAG` | `latest` | Tag của cả hai image |
| `CONTROLLER_IMG` | `$(REGISTRY)/controller:$(TAG)` | Ghi đè hẳn tên image controller |
| `CNI_IMG` | `$(REGISTRY)/cni:$(TAG)` | Ghi đè hẳn tên image CNI |
| `CONTAINER_TOOL` | `docker` | Đổi thành `podman` nếu cần |
| `TARGETARCH` | `amd64` | Kiến trúc đích của `build-linux` |
| `KURYR_CONF` | `etc/kuryr.conf` | File cấu hình mà `make run` dùng |
| `ENVTEST_K8S_VERSION` | `1.37.0` | Phiên bản control plane cho envtest |
| `LOCALBIN` | `./bin`, hoặc dự phòng | Nơi tải công cụ về. Xem §6 |
| `HELM_CHART_DIR` | `dist/kuryr` | Thư mục **và tên** chart sinh ra |

---

## 3. Phát triển

| Target | Làm gì |
| --- | --- |
| `manifests` | Sinh CRD và ClusterRole từ marker kubebuilder trong mã nguồn |
| `generate` | Sinh các hàm `DeepCopy` cho `api/v1` |
| `fmt` | `go fmt ./...` |
| `vet` | `go vet ./...` **và** `GOOS=linux go vet ./...` |
| `test` | Unit test + envtest, kèm coverage. Tự chạy `manifests generate fmt vet` trước |
| `test-race` | Chạy lại dưới race detector. Cần cgo |
| `test-e2e` | Chạy bộ e2e trên cụm kind đã có sẵn |
| `setup-test-e2e` | Tạo cụm kind tên `kuryr-e2e` |
| `cleanup-test-e2e` | Xoá cụm đó |
| `lint` | `golangci-lint run` |
| `lint-fix` | `golangci-lint run --fix` |
| `check` | `test` + `lint` |
| `verify-generated` | Thất bại nếu file sinh ra chưa được commit |

### Vì sao `vet` chạy hai lần

Tất cả phần chạm tới netlink đều nằm sau build tag `linux`. Người phát triển
trên macOS hoặc Windows **không bao giờ biên dịch nửa CNI daemon đó** — nên một
lỗi biên dịch ở đấy lọt qua toàn bộ vòng lặp phát triển và chỉ lộ ra trong CI,
hoặc tệ hơn là lúc build image.

Vet cho `GOOS=linux` một cách tường minh là thứ giữ nửa ấy trung thực. Nên chạy
lint tương tự trước khi tin vào một kết quả sạch:

```sh
GOOS=linux golangci-lint run
```

### `verify-generated` dùng khi nào

Cụm lấy CRD từ `config/crd`, **không** phải từ các marker trong mã nguồn. Một
thay đổi schema quên regenerate là một phân kỳ âm thầm: mã nguồn nói một đằng,
cụm thi hành một nẻo, và chỉ lộ ra khi API server từ chối một object.

Đặt target này vào CI, sau `make manifests generate`.

---

## 4. Build

| Target | Làm gì |
| --- | --- |
| `build` | Bốn binary vào `bin/`, **cho máy hiện tại** |
| `build-linux` | Bốn binary cross-compile cho Linux vào `bin/linux_$(TARGETARCH)/` |
| `run` | Chạy controller với kubeconfig hiện tại và `$(KURYR_CONF)` |
| `docker-build` | Build cả hai image |
| `docker-push` | Push cả hai image |
| `kind-load` | Nạp cả hai image vào cụm kind `kuryr-e2e` |
| `build-installer` | Render toàn bộ triển khai thành `dist/install.yaml` |
| `helm` | Render thành Helm chart ở `dist/kuryr` |

### `build` và `build-linux` khác nhau ở đâu

`build` build cho máy bạn — đó là thứ `go run`, debugger và test cần.

`build-linux` mới là thứ **copy lên node được**. `kuryr-daemon` và `kuryr-cni`
dùng netlink và network namespace; trên nền tảng khác Linux chúng biên dịch với
stub từ chối mọi lời gọi. Binary cross-compile còn đi kèm `CGO_ENABLED=0`,
`-trimpath` và `-ldflags="-s -w"` để chạy được trên base image không có libc.

### `kind-load` để làm gì

Kind đọc image từ containerd của chính nó, không phải từ daemon Docker trên
host. Một image vừa build ở máy là **vô hình** với kind cho tới khi được nạp
vào. Target này làm cho vòng lặp build → deploy → test chạy được mà không cần
registry nào cả:

```sh
make docker-build REGISTRY=local TAG=dev
make kind-load    REGISTRY=local TAG=dev
make deploy CONTROLLER_IMG=local/controller:dev CNI_IMG=local/cni:dev
```

### Chart Helm là sản phẩm sinh ra

`make helm` xoá `dist/kuryr` rồi render lại từ đầu — helmify chỉ biết *thêm*
vào chart, nên một template cho tài nguyên không còn tồn tại sẽ sống sót qua
lần regenerate và tiếp tục được apply.

helmify không diễn đạt được **hai** service account, nên chart nó viết ra sẽ
trao quyền của controller cho node agent privileged. Script
`hack/helm-split-serviceaccounts.sh` chạy ngay sau đó để tách lại — **nó không
phải tuỳ chọn**.

Đừng sửa tay trong `dist/kuryr`. Sửa nguồn ở `config/` rồi regenerate.

---

## 5. Triển khai

| Target | Làm gì |
| --- | --- |
| `install` | Chỉ cài CRD vào cụm |
| `uninstall` | Gỡ CRD |
| `deploy` | Cài controller + CNI agent + RBAC + ConfigMap |
| `undeploy` | Gỡ tất cả những thứ trên |

Bỏ qua lỗi "không tìm thấy" khi gỡ:

```sh
make undeploy ignore-not-found=true
```

> `uninstall` xoá CRD, và cùng với nó là mọi KuryrPort, KuryrNetwork,
> KuryrNetworkPolicy, KuryrLoadBalancer. Finalizer của chúng chính là thứ giải
> phóng tài nguyên OpenStack — chạy khi controller đã bị gỡ sẽ bỏ lại port,
> security group và load balancer trong đám mây. Xem
> [installation.vi.md §12](installation.vi.md#12-nâng-cấp-và-gỡ-bỏ).

---

## 6. Công cụ phụ thuộc

Mọi công cụ đều **ghim phiên bản**, để một bản build tái lập được và một lần
nâng cấp là một diff xem xét được, chứ không phải điều tự xảy ra lần sau CI chạy.

| Công cụ | Biến phiên bản | Dùng để |
| --- | --- | --- |
| controller-gen | `CONTROLLER_TOOLS_VERSION` | Sinh CRD, RBAC, DeepCopy |
| kustomize | `KUSTOMIZE_VERSION` | Render manifest |
| setup-envtest | `ENVTEST_VERSION` | Tải binary control plane cho test |
| golangci-lint | `GOLANGCI_LINT_VERSION` | Lint |
| kind | `KIND_VERSION` | Cụm e2e |
| helmify | `HELMIFY_VERSION` | Sinh chart |

Chúng được tải về `$(LOCALBIN)` khi dùng lần đầu, tên file có kèm phiên bản, để
chuyển nhánh thì cài lại chứ không âm thầm dùng nhầm binary cũ.

### Đường dẫn có dấu cách

GNU make **không thể** đặt tên một target chứa dấu cách. Checkout nằm trong
thư mục kiểu `C:/Users/me/My Documents/kuryr-go`, hay một thư mục OneDrive /
Dropbox được đồng bộ, sẽ làm mọi target công cụ vỡ.

Makefile phát hiện việc này và tự chuyển `LOCALBIN` sang
`$(GOPATH)/bin/kuryr-tools`, kèm một dòng thông báo:

```
this checkout is under a path containing a space, so tools go to /home/me/go/bin/kuryr-tools instead of ./bin
```

Muốn chỉ định chỗ khác:

```sh
make test LOCALBIN=/tmp/kuryr-bin
```

### Trên Windows

- Chạy `make` **trong Git Bash**. Makefile dùng cú pháp POSIX sh
  (`SHELL = /usr/bin/env bash -o pipefail`).
- Go thêm đuôi `.exe` cho binary trên Windows; Makefile đã tính tới điều này
  qua biến `EXE`, nếu không thì mỗi lần chạy sẽ cài lại công cụ vì file nó tìm
  không bao giờ là file `go install` vừa ghi ra.
- `make build` sinh binary Windows. Muốn thứ chạy được trên node thì dùng
  `make build-linux`.
- Đường dẫn Windows mà `go env GOPATH` trả về dùng dấu `\`, còn shell của
  recipe là `sh` sẽ nuốt mất chúng — Makefile chuẩn hoá sang `/` trước khi dùng.

---

## 7. Chạy thử toàn bộ, đầu tới cuối

Không cần OpenStack:

```sh
make check                                    # sinh mã, vet, test, lint
make setup-test-e2e                           # cụm kind
make test-e2e                                 # kiểm CRD trên API server thật
make docker-build REGISTRY=local TAG=dev      # hai image
make kind-load    REGISTRY=local TAG=dev      # nạp vào kind
make deploy CONTROLLER_IMG=local/controller:dev CNI_IMG=local/cni:dev
kubectl -n kube-system get pods -l app.kubernetes.io/name=kuryr
make cleanup-test-e2e                         # dọn
```

Diễn giải kết quả ở
[installation.vi.md §11](installation.vi.md#11-thử-trên-kind-không-cần-openstack).

---

## Đọc tiếp

| Tài liệu | Nội dung |
| --- | --- |
| [openstack-primer.vi.md](openstack-primer.vi.md) | Các thành phần OpenStack, cho người mới |
| [installation.vi.md](installation.vi.md) | Cài đặt từ đầu |
| [architecture.md](architecture.md) | Kiến trúc chi tiết, tiếng Anh |
| [configuration.md](configuration.md) | Từng khoá `kuryr.conf`, tiếng Anh |
