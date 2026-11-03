# Hướng dẫn cài đặt

Tài liệu này đi từ con số không tới một cụm Kubernetes chạy Kuryr làm CNI plugin.

Nếu chưa quen các khái niệm OpenStack (project, network, subnet, port, security
group, trunk, Octavia…), đọc [openstack-primer.vi.md](openstack-primer.vi.md)
trước — tài liệu này giả định bạn đã biết chúng là gì.

**Mục lục**

1. [Điều kiện cần](#1-điều-kiện-cần)
2. [Chuẩn bị tài nguyên OpenStack](#2-chuẩn-bị-tài-nguyên-openstack)
3. [Chuẩn bị cụm Kubernetes](#3-chuẩn-bị-cụm-kubernetes)
4. [Build image](#4-build-image)
5. [Cấu hình](#5-cấu-hình)
6. [Kiểm tra trước khi cài](#6-kiểm-tra-trước-khi-cài)
7. [Triển khai bằng kustomize](#7-triển-khai-bằng-kustomize)
8. [Triển khai bằng Helm](#8-triển-khai-bằng-helm)
9. [Kiểm chứng](#9-kiểm-chứng)
10. [Gỡ lỗi](#10-gỡ-lỗi)
11. [Thử trên kind, không cần OpenStack](#11-thử-trên-kind-không-cần-openstack)
12. [Nâng cấp và gỡ bỏ](#12-nâng-cấp-và-gỡ-bỏ)

---

## 1. Điều kiện cần

### Phía OpenStack

| Thành phần | Bắt buộc? | Ghi chú |
| --- | --- | --- |
| Keystone | Có | Cần một project mà Kuryr được phép tạo port trong đó |
| Neutron | Có | |
| Octavia | Chỉ khi cần Service | Không có thì bỏ cặp handler `service,kuryrloadbalancer` |
| Nova | Chỉ với `nested-dpdk` | |
| Extension `trunk` của Neutron | Chỉ với `nested-vlan` | Kiểm tra: `openstack extension list --network \| grep trunk` |
| `max_allowed_address_pair` đủ lớn | Chỉ với `nested-macvlan` | Mỗi pod chiếm một pair trên port VM node; mặc định 10. Đặt trong `neutron.conf` |

### Phía Kubernetes

- Container runtime nạp CNI plugin từ `/etc/cni/net.d` và `/opt/cni/bin` —
  đây là mặc định của containerd và CRI-O.
- Các node phải tới được **cả** API Kubernetes **và** API OpenStack.
- **Chưa có CNI plugin nào khác được cài.** Kuryr ghi ra
  `10-kuryr.conflist`, còn runtime chọn file sắp xếp trước nhất theo tên — nên
  để hai plugin cùng tồn tại là để ngỏ cho may rủi.

### Trên máy làm việc

- Go 1.27 trở lên (chỉ cần nếu build từ mã nguồn)
- Docker hoặc Podman (để build image)
- `kubectl`
- GNU Make. Trên Windows dùng Git Bash — Makefile viết bằng cú pháp POSIX sh.

---

## 2. Chuẩn bị tài nguyên OpenStack

Các lệnh dưới dùng OpenStack CLI. Ghi lại các UUID in ra — chúng sẽ đi vào
`kuryr.conf`.

### 2.1 Project và thông tin xác thực

```sh
# Xem project hiện tại
openstack project show -f value -c id
```

Tạo application credential thay vì dùng mật khẩu — nó thu hồi được riêng lẻ và
không chết khi người dùng đổi mật khẩu:

```sh
openstack application credential create kuryr \
  --description "kuryr-kubernetes controller" \
  --unrestricted
```

Lệnh in ra `id` và `secret`. **`secret` chỉ hiện đúng một lần**, hãy lưu ngay.

Tài khoản này cần quyền tạo/xoá: port, network, subnet, security group,
security group rule, floating IP, và (nếu dùng Service) load balancer Octavia.
Vai trò `member` trên project thường là đủ.

### 2.2 Mạng và subnet cho pod

```sh
openstack network create kuryr-pod-net

openstack subnet create kuryr-pod-subnet \
  --network kuryr-pod-net \
  --subnet-range 10.10.0.0/16
```

### 2.3 Subnet cho Service

Subnet này **không có gì sống trên đó**. Nó tồn tại để Octavia giữ được VIP tại
đúng địa chỉ `clusterIP` mà Kubernetes cấp cho từng Service.

> **Đây là chỗ sai phổ biến nhất khi cài lần đầu.** Dải của subnet này phải
> khớp với `--service-cluster-ip-range` của kube-apiserver. Lệch nhau thì mọi
> Service đều hỏng với thông báo `clusterIP ... is not in any service subnet`.

```sh
# Xem service CIDR mà cụm đang dùng
kubectl -n kube-system get pod -l component=kube-apiserver \
  -o jsonpath='{.items[0].spec.containers[0].command}' | tr ',' '\n' | grep service-cluster-ip-range

openstack network create kuryr-service-net

openstack subnet create kuryr-service-subnet \
  --network kuryr-service-net \
  --subnet-range 10.96.0.0/12 \
  --no-dhcp
```

### 2.4 Router

Nối hai subnet trên với nhau và ra ngoài:

```sh
openstack router create kuryr-router
openstack router set kuryr-router --external-gateway <mạng-ngoài>
openstack router add subnet kuryr-router kuryr-pod-subnet
openstack router add subnet kuryr-router kuryr-service-subnet
```

### 2.5 Security group mặc định cho pod

Đây là nhóm mà pod nhận khi **không** NetworkPolicy nào chọn nó. Với
`pod_security_groups_driver = policy`, nó vẫn bắt buộc phải có.

```sh
openstack security group create kuryr-pods

# Cho phép pod nói chuyện với nhau
openstack security group rule create kuryr-pods \
  --remote-ip 10.10.0.0/16 --protocol any

# Cho phép node (kubelet) tới được pod, nếu không probe sẽ hỏng
openstack security group rule create kuryr-pods \
  --remote-ip <CIDR-mạng-node> --protocol any
```

### 2.6 Nếu dùng subnet riêng cho từng namespace

Chỉ cần khi đặt `pod_subnets_driver = namespace`:

```sh
openstack subnet pool create kuryr-pod-pool \
  --pool-prefix 10.20.0.0/16 --default-prefix-length 24
```

Khi đó `pod_subnet` không còn bắt buộc, nhưng `[namespace_subnet]pod_router`
và `pod_subnet_pool` thì có.

### 2.7 Tổng kết những gì cần ghi lại

| Giá trị | Lấy bằng |
| --- | --- |
| project id | `openstack project show -f value -c id` |
| pod subnet id | `openstack subnet show kuryr-pod-subnet -f value -c id` |
| service subnet id | `openstack subnet show kuryr-service-subnet -f value -c id` |
| security group id | `openstack security group show kuryr-pods -f value -c id` |
| router id | `openstack router show kuryr-router -f value -c id` |
| mạng ngoài id | `openstack network list --external -f value -c ID` |
| subnet pool id | `openstack subnet pool show kuryr-pod-pool -f value -c id` |

---

## 3. Chuẩn bị cụm Kubernetes

Kuryr **là** CNI plugin của cụm, nên nó phải lên được trước khi bất kỳ pod nào
có địa chỉ. Điều đó kéo theo vài ràng buộc, và các manifest đi kèm đã xử lý sẵn:

- Mọi thứ nằm ở `kube-system`, cùng chỗ với control plane.
- Controller và daemon đều `hostNetwork: true` — chúng không dùng được chính
  mạng pod mà chúng chịu trách nhiệm tạo ra.
- Cả hai `tolerations: [{operator: Exists}]` — phải chạy được trên node đang
  `NotReady` **chính vì** chưa có CNI plugin nào hoạt động.
- Controller dùng `priorityClassName: system-cluster-critical`, daemon dùng
  `system-node-critical`.

Nếu cụm bật Pod Security Admission trên `kube-system`, gán nhãn trước khi cài —
node agent buộc phải chạy privileged:

```sh
kubectl label ns kube-system pod-security.kubernetes.io/enforce=privileged --overwrite
```

---

## 4. Build image

Có hai image:

| Image | Chứa | Base |
| --- | --- | --- |
| `controller` | `kuryr-controller`, `kuryr-status` | distroless (không shell) |
| `cni` | `kuryr-daemon`, `kuryr-cni`, script cài plugin | CentOS Stream 9 + Open vSwitch |

Image CNI **không** distroless được: daemon gọi `ovs-vsctl` để cắm port vào
integration bridge, còn script khởi tạo cần shell để cài plugin lên host.

```sh
make docker-build docker-push \
  CONTROLLER_IMG=<registry>/controller:<tag> \
  CNI_IMG=<registry>/cni:<tag>
```

Hoặc đặt một lần:

```sh
make docker-build docker-push REGISTRY=<registry> TAG=<tag>
```

Chỉ muốn binary, không cần image:

```sh
make build-linux          # cross-compile vào bin/linux_amd64/
make build-linux TARGETARCH=arm64
```

`make build` (không có `-linux`) build cho chính máy bạn — dùng để chạy test và
debug. Đừng copy kết quả đó lên node nếu bạn không ở trên Linux:
`kuryr-daemon` và `kuryr-cni` dùng netlink và network namespace, trên nền tảng
khác chúng biên dịch với stub từ chối mọi lời gọi.

---

## 5. Cấu hình

Cả hai binary đọc **cùng một** file `kuryr.conf`. Controller bỏ qua mục
`[cni_daemon]`, daemon bỏ qua hầu hết `[kubernetes]`; không có gì trong file là
riêng cho từng binary.

Có hai bản trong repo, dùng cho hai việc khác nhau:

| File | Dùng khi |
| --- | --- |
| [`etc/kuryr.conf`](../etc/kuryr.conf) | Chạy controller từ máy làm việc (`make run`). Là bản mẫu có chú thích để copy |
| [`config/manager/kuryr_config.yaml`](../config/manager/kuryr_config.yaml) | ConfigMap mà controller và daemon **trong cụm** thật sự đọc |

**Sửa file thứ hai trước khi triển khai.** Sửa bản mẫu không làm thay đổi gì
trên cụm đang chạy.

### 5.1 Các giá trị bắt buộc

Trong `config/manager/kuryr_config.yaml`, thay các chỗ đánh dấu:

```ini
[neutron_defaults]
project             = <project id ở §2.1>
pod_subnet          = <pod subnet id ở §2.2>
service_subnet      = <service subnet id ở §2.3>
pod_security_groups = <security group id ở §2.5>

[namespace_subnet]
pod_router      = <router id ở §2.4>
pod_subnet_pool = <subnet pool id ở §2.6>
```

Mô tả từng khoá nằm ở [configuration.md](configuration.md).

### 5.2 Thông tin xác thực

Giống kuryr bản Python, thông tin Keystone nằm trong mục `[neutron]` của
`kuryr.conf`. Điền vào ConfigMap:

```ini
[neutron]
auth_url = https://keystone.example.com:5000/v3
username = <user>
password = <password>
project_name = <project>
user_domain_name = Default
project_domain_name = Default
region_name = RegionOne
```

Hoặc dùng application credential ở §2.1:

```ini
[neutron]
auth_url = https://keystone.example.com:5000/v3
application_credential_id = <id>
application_credential_secret = <secret>
region_name = RegionOne
```

Khoá nào để trống sẽ lấy từ biến môi trường `OS_*` tương ứng (`OS_AUTH_URL`,
`OS_PASSWORD`…), nếu tiến trình được chạy kèm các biến đó.

### 5.3 Chọn cấu hình theo hoàn cảnh

| Node là gì | `pod_vif_driver` | Cần thêm |
| --- | --- | --- |
| Máy vật lý | `neutron-vif` | — |
| Máy ảo OpenStack | `nested-vlan` | Extension `trunk`; `[pod_vif_nested]worker_nodes_subnets` |
| Máy ảo, không có trunk | `nested-macvlan` | `vif_pool_driver = noop` bắt buộc |
| Workload NFV/DPDK | `nested-dpdk` | Nova trong catalog; `vif_pool_driver = noop` |

Tập handler tối thiểu chỉ cần pod có mạng và Service chạy được:

```ini
enabled_handlers = vif,kuryrport,service,kuryrloadbalancer
```

Thêm từng cặp khi cần:

| Thêm cặp | Được gì | Kéo theo |
| --- | --- | --- |
| `namespace,kuryrnetwork` | Mỗi namespace một subnet Neutron | `pod_subnets_driver = namespace`, `pod_router` |
| `policy,kuryrnetworkpolicy` | NetworkPolicy thành security group | `pod_security_groups_driver = policy` **và** `service_security_groups_driver = policy` |

Bật nửa cặp sẽ bị **từ chối ngay lúc khởi động**, chứ không âm thầm không làm gì.

---

## 6. Kiểm tra trước khi cài

`kuryr-status` báo cái gì sai **khi còn kịp sửa**, thay vì sau khi controller
đã vào crash loop. Nó nằm sẵn trong image controller nên chạy được với đúng
cấu hình mà controller sẽ đọc.

Chỉ kiểm tra cấu hình, không cần cụm và không cần đám mây:

```sh
kuryr-status --config-file=etc/kuryr.conf --config-only
```

Kiểm tra tất cả — cấu hình, CRD, endpoint và extension mà các driver đã chọn cần:

```sh
kuryr-status --config-file=/etc/kuryr/kuryr.conf
```

Chạy từ image, không cần cài gì lên máy:

```sh
docker run --rm -v "$PWD/etc:/etc/kuryr:ro" \
  --entrypoint /kuryr-status <registry>/controller:<tag> \
  --config-file=/etc/kuryr/kuryr.conf --config-only
```

Kết quả:

```
Upgrade check results
------------------------------
Configuration      Success
Ports pool sizing  Success
```

Mã thoát: `0` nếu mọi thứ ổn, `1` nếu có cảnh báo, `2` nếu có lỗi — dùng thẳng
trong pipeline được.

---

## 7. Triển khai bằng kustomize

```sh
make deploy \
  CONTROLLER_IMG=<registry>/controller:<tag> \
  CNI_IMG=<registry>/cni:<tag>
```

Lệnh này apply `config/default`, gồm: 4 CRD, RBAC, ConfigMap, Deployment của
controller (1 replica), DaemonSet của CNI agent, và Service cho
metrics. Tất cả vào `kube-system`, tên có tiền tố `kuryr-`.

Muốn xem trước mà chưa apply:

```sh
make build-installer CONTROLLER_IMG=... CNI_IMG=...
less dist/install.yaml
```

File đó apply trực tiếp được, không cần kustomize trên máy đích:

```sh
kubectl apply -f dist/install.yaml
```

### Số replica và HA

Mặc định controller chạy **1 replica**, giống manifest của kuryr bản Python,
với `RollingUpdate` `maxSurge: 0`, `maxUnavailable: 1`.

Muốn HA active/passive, tăng số replica. Leader election đảm bảo chỉ một
controller đối soát: hai controller cùng xử lý một pod sẽ cùng tạo Neutron port
và rò rỉ một port. Hai điều cần giữ:

- **Số replica không vượt số node có thể xếp lịch.** Controller dùng host
  network, nên mỗi node chỉ chạy được một pod controller. Replica thừa sẽ nằm
  `Pending` mãi và làm rollout bị kẹt.
- **Giữ `maxSurge: 0`.** Pod mới chỉ lên được khi một pod cũ đã nhường node.

---

## 8. Triển khai bằng Helm

```sh
make helm CONTROLLER_IMG=<registry>/controller:<tag> CNI_IMG=<registry>/cni:<tag>
helm install kuryr ./dist/kuryr --namespace kube-system
```

`make helm` render chính output của kustomize thành chart ở `dist/kuryr`.

> Chart là **sản phẩm sinh ra**, không phải mã nguồn. Sửa trực tiếp trong đó sẽ
> mất khi regenerate. Có một thứ trình sinh không tự diễn đạt được — việc tách
> hai service account — nên `make helm` chạy thêm
> `hack/helm-split-serviceaccounts.sh` ngay sau; xem
> [architecture.md](architecture.md#helm-chart-generation).

---

## 9. Kiểm chứng

### 9.1 Các tiến trình đã lên chưa

```sh
kubectl -n kube-system get deploy,ds -l app.kubernetes.io/part-of=kuryr
```

Mong đợi: DaemonSet `READY` bằng số node; Deployment `1/1`.

### 9.2 Plugin đã được cài lên host chưa

DaemonSet copy binary plugin lên từng node. Kiểm tra trên một node bất kỳ:

```sh
ls -l /opt/cni/bin/kuryr-cni
cat /etc/cni/net.d/10-kuryr.conflist
```

### 9.3 Daemon có khoẻ không

```sh
curl -s http://127.0.0.1:8090/healthz     # 200
curl -s http://127.0.0.1:8090/readyz      # 200
curl -s http://127.0.0.1:9655/metrics | grep kuryr_cni
```

`kuryr_cni_tracked_pods` phải bằng số pod (không phải hostNetwork) trên node đó.

### 9.4 Pod có lấy được port Neutron không

```sh
kubectl create deployment test --image=nginx --replicas=2
kubectl get pods -o wide
kubectl get kuryrports -A
```

Đối chiếu phía OpenStack:

```sh
openstack port list --tags kuryr
```

### 9.5 Service có ra load balancer không

```sh
kubectl expose deployment test --port=80
kubectl get kuryrloadbalancers -A
openstack loadbalancer list
```

Với Service kiểu `LoadBalancer`, `EXTERNAL-IP` sẽ hiện floating IP khi Octavia
xong — mất hàng chục giây với provider amphora, đó là bình thường.

---

## 10. Gỡ lỗi

### Nguyên tắc chung

Mọi CRD đều mang `conditions`. Pod kẹt `ContainerCreating` **luôn** có lời giải
thích trên object, không phải chỉ trong log:

```sh
kubectl describe kuryrport -n <namespace> <tên-pod>
```

### Bảng triệu chứng

| Triệu chứng | Nguyên nhân thường gặp | Cách xử lý |
| --- | --- | --- |
| Controller crash loop, log `no auth_url configured` hoặc `authenticating to ...` | Mục `[neutron]` chưa điền hoặc sai | §5.2 |
| Controller crash loop với thông báo về handler | Bật nửa cặp handler | §5.3 |
| Pod kẹt `ContainerCreating`, KuryrPort có `SubnetNotReady` | Subnet của namespace đang được tạo | Tự khỏi. Nếu kéo dài, xem `kubectl get kuryrnetwork -A` |
| KuryrPort có `SubnetUnavailable` | Annotation trỏ tới subnet không tồn tại hoặc thuộc project khác | Đọc `message` của condition. Không tự khỏi |
| KuryrPort có `PortStuckInactive` | Neutron không đưa được port lên ACTIVE | Kiểm tra agent OVS trên node; xem `binding:vif_type` có phải `binding_failed` |
| Không có KuryrPort nào được tạo | Handler `vif` chưa bật, hoặc pod là hostNetwork | `kubectl -n kube-system logs deploy/kuryr-controller-manager \| head` |
| CNI báo `cannot reach kuryr-daemon` | DaemonSet chưa chạy trên node đó | `kubectl -n kube-system get pods -o wide \| grep kuryr-cni` |
| CNI timeout chờ port | Controller không tới được Neutron | Xem log controller; xem `/readyz` của controller |
| Mọi Service hỏng, `clusterIP ... is not in any service subnet` | `service_subnet` không khớp service CIDR của cụm | §2.3 |
| Service `LoadBalancer` báo không hỗ trợ | Chưa đặt `external_svc_net` | §2.7 và [configuration.md](configuration.md) |
| Pod ping được nhưng TLS treo | MTU mạng pod lớn hơn MTU uplink node | Giảm MTU của Neutron network |
| Pod macvlan không gửi được gói | Thiếu allowed address pair | Kuryr tự thêm; nếu lỗi sẽ có trong log controller |
| Pod macvlan thứ 11 trên một node lỗi `AllowedAddressPairExhausted` | Port VM node đã dùng hết `max_allowed_address_pair` (mặc định 10) | Tăng `max_allowed_address_pair` trong `[DEFAULT]` của `neutron.conf` rồi restart neutron-server |

### Các lệnh hay dùng

```sh
# Log controller đang là leader
kubectl -n kube-system logs deploy/kuryr-controller-manager -f

# Log daemon trên một node cụ thể
kubectl -n kube-system logs -l app.kubernetes.io/component=cni \
  --field-selector spec.nodeName=<node> -f

# Toàn cảnh trạng thái Kuryr
kubectl get kuryrports,kuryrnetworks,kuryrnetworkpolicies,kuryrloadbalancers -A

# Tài nguyên Kuryr đã tạo bên OpenStack (nhờ resource_tags)
openstack port list --tags kuryr
openstack security group list --tags kuryr
openstack network list --tags kuryr
```

### Khi controller không tới được Neutron

Readiness probe của controller **có** kiểm tra Neutron, nên một đợt rollout sẽ
dừng lại chứ không đi tiếp vào giữa một sự cố OpenStack. Đây là chủ ý.

Trong lúc đó, **pod đang chạy không bị ảnh hưởng**: daemon không nói chuyện với
OpenStack bao giờ. Chỉ pod mới là không tạo được.

---

## 11. Thử trên kind, không cần OpenStack

Không có đám mây vẫn kiểm tra được khá nhiều: schema CRD, RBAC, việc cài plugin
lên host, daemon lên được không, endpoint metrics, và thông báo lỗi của
controller khi thiếu cấu hình.

```sh
# 1. Dựng cụm
make setup-test-e2e

# 2. Kiểm tra CRD trên một API server thật
make test-e2e

# 3. Build image và nạp thẳng vào kind (không cần registry)
make docker-build REGISTRY=local TAG=dev
make kind-load    REGISTRY=local TAG=dev

# 4. Triển khai
make deploy CONTROLLER_IMG=local/controller:dev CNI_IMG=local/cni:dev

# 5. Xem kết quả
kubectl -n kube-system get pods -l app.kubernetes.io/name=kuryr
```

Kết quả **mong đợi**, không phải lỗi:

- `kuryr-cni` DaemonSet: `Running`, informer đồng bộ, HTTP server nghe ở
  `127.0.0.1:5036`, metrics ở `:9655`.
- `kuryr-controller-manager`: `CrashLoopBackOff`, log dừng ở dòng
  `openstack: authenticating to <keystone-url>: ...` vì ConfigMap mẫu còn
  placeholder. Không có OpenStack thì controller **phải** từ chối chạy.

Kind dùng kindnet làm CNI, và `10-kindnet.conflist` sắp trước
`10-kuryr.conflist` theo thứ tự chữ cái, nên mạng của cụm vẫn chạy bình thường
trong lúc thử.

Thử trực tiếp giao thức HTTP của daemon:

```sh
docker exec <node-kind> curl -s -X POST \
  -H 'Content-Type: application/json' \
  -d '{"CNI_COMMAND":"ADD","CNI_IFNAME":"eth0","CNI_ARGS":""}' \
  http://127.0.0.1:5036/addNetwork
# {"code":4,"msg":"required CNI parameters missing",...}
```

Dọn dẹp:

```sh
make cleanup-test-e2e
```

---

## 12. Nâng cấp và gỡ bỏ

### Nâng cấp

```sh
make docker-build docker-push REGISTRY=<registry> TAG=<tag mới>
make deploy REGISTRY=<registry> TAG=<tag mới>
```

Với `maxSurge: 0`, pod controller cũ bị xoá trước rồi pod mới mới được tạo, nên
trong vài giây sẽ không có controller. Pod đang chạy không bị ảnh hưởng; pod
được tạo trong khoảng đó chờ CNI thử lại.

DaemonSet cập nhật với `maxUnavailable: 1`. Trong lúc daemon trên một node khởi
động lại, CNI ADD trên node đó sẽ thất bại và được kubelet thử lại — pod đang
chạy không bị động tới.

### Gỡ bỏ

```sh
make undeploy    # bỏ controller, daemon và RBAC
make uninstall   # bỏ luôn CRD
```

> **Bỏ CRD sẽ xoá mọi KuryrPort, KuryrNetwork, KuryrNetworkPolicy và
> KuryrLoadBalancer.** Finalizer của chúng là thứ giải phóng tài nguyên
> OpenStack; xoá CRD khi controller đã bị gỡ sẽ **bỏ lại** port, security group
> và load balancer trong đám mây.
>
> Thứ tự đúng: xoá hết workload trước → chờ KuryrPort biến mất hết → rồi mới
> `make undeploy` và `make uninstall`.

Dọn tay những gì còn sót (nhờ `resource_tags`):

```sh
openstack port list --tags kuryr -f value -c ID | xargs -r -n1 openstack port delete
openstack security group list --tags kuryr -f value -c ID | xargs -r -n1 openstack security group delete
```

---

## Đọc tiếp

| Tài liệu | Nội dung |
| --- | --- |
| [openstack-primer.vi.md](openstack-primer.vi.md) | Các thành phần OpenStack, cho người mới |
| [makefile.vi.md](makefile.vi.md) | Toàn bộ target của Makefile |
| [configuration.md](configuration.md) | Từng khoá `kuryr.conf`, tiếng Anh |
| [architecture.md](architecture.md) | Kiến trúc chi tiết, tiếng Anh |
