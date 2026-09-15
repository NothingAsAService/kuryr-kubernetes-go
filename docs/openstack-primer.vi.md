# OpenStack cho người chưa biết gì

Tài liệu này dành cho người đã quen Kubernetes nhưng **chưa từng dùng
OpenStack**. Nó giải thích từng thành phần OpenStack mà Kuryr đụng tới, tại sao
nó tồn tại, và nó tương ứng với khái niệm nào trong Kubernetes.

Đọc xong tài liệu này thì [architecture.md](architecture.md) — mô tả kiến trúc
Kuryr — sẽ đọc được. Còn để dựng một cụm thật, xem
[installation.vi.md](installation.vi.md).

> Mọi thuật ngữ OpenStack đều giữ nguyên tiếng Anh, vì đó là tên gọi trong API,
> trong CLI và trong log. Bảng đối chiếu Việt–Anh ở [cuối trang](#bảng-thuật-ngữ).

---

## 1. Vấn đề Kuryr giải quyết

Kubernetes không tự làm mạng. Nó định nghĩa một giao ước — *mọi pod phải có một
địa chỉ IP và ping được nhau* — rồi giao việc thực hiện cho một **CNI plugin**
(Calico, Cilium, Flannel…).

Khi cụm Kubernetes chạy **trên** OpenStack, ta rơi vào cảnh có hai lớp mạng ảo
chồng lên nhau:

```
   Pod A  ──►  overlay (VXLAN của Flannel/Calico)  ──►  Pod B
                            │
                            ▼
              mạng ảo của OpenStack (Neutron)  ──►  mạng vật lý
```

Gói tin bị đóng gói hai lần. Hệ quả:

- **Chậm.** Mỗi lớp đóng gói ăn CPU và cắt bớt MTU.
- **Không nhìn thấy gì.** Với OpenStack, toàn bộ traffic giữa các pod chỉ là
  một dòng dữ liệu vô danh giữa hai máy ảo. Security group, quota, kiểm toán,
  giám sát của OpenStack đều bị mù.
- **Hai nơi quản trị.** Firewall của OpenStack và NetworkPolicy của Kubernetes
  là hai hệ thống khác nhau, không biết đến nhau.

**Kuryr bỏ lớp overlay.** Mỗi pod nhận một **Neutron port** thật — giống hệt
một máy ảo. Với OpenStack, pod và VM là công dân hạng nhất như nhau.

```
   Pod A  ──►  Neutron port  ──►  mạng vật lý  ──►  Neutron port  ──►  Pod B
```

---

## 2. Bản đồ các dịch vụ OpenStack

OpenStack không phải một phần mềm mà là một tập hợp dịch vụ, mỗi dịch vụ có API
riêng. Kuryr chỉ dùng bốn:

| Dịch vụ | Tên mã | Làm gì | Kuryr dùng để |
| --- | --- | --- | --- |
| Identity | **Keystone** | Xác thực, phân quyền, danh bạ endpoint | Đăng nhập, tìm địa chỉ các API khác |
| Networking | **Neutron** | Mạng ảo: network, subnet, port, router, firewall | Cấp IP và card mạng cho pod |
| Load Balancing | **Octavia** | Cân bằng tải như một dịch vụ | Hiện thực Kubernetes Service |
| Compute | **Nova** | Quản lý máy ảo | Chỉ driver `nested-dpdk` mới cần |

Có thể hình dung: **Keystone ≈ IAM + service discovery**, **Neutron ≈ VPC**,
**Octavia ≈ Load Balancer service**, **Nova ≈ EC2**.

---

## 3. Keystone — danh tính

Keystone trả lời hai câu hỏi: *bạn là ai* và *các API khác nằm ở đâu*.

### Project (tenant)

Đơn vị cô lập tài nguyên. Mọi thứ — máy ảo, mạng, IP — đều **thuộc về đúng một
project**. Hạn ngạch (quota) tính theo project. Người dùng ở project khác không
nhìn thấy tài nguyên của bạn.

> **Tương đương Kubernetes:** gần giống Namespace, nhưng cứng hơn nhiều. Ranh
> giới Namespace chỉ là quy ước trong một API server; ranh giới project là ranh
> giới của cả một đám mây.

Kuryr tạo mọi thứ trong project cấu hình ở `[neutron_defaults]project`. Cụm
nhiều tenant có thể để mỗi Kubernetes namespace trỏ sang một project riêng bằng
annotation `openstack.org/kuryr-project`.

### Domain

Một tầng nữa trên project, dùng để tách các tổ chức độc lập. Đa số cụm chỉ có
một domain tên `Default` — đó là lý do `user_domain_name = Default` xuất hiện
trong mọi file cấu hình.

### Xác thực: mật khẩu và application credential

Cách cũ là username + password. Cách nên dùng cho dịch vụ chạy dài là
**application credential**: một cặp id/secret

- gắn với một người dùng nhưng **thu hẹp được quyền**,
- **thu hồi được riêng lẻ**, không đụng tới mật khẩu của người đó,
- không hết hạn khi người đó đổi mật khẩu.

Kuryr là tiến trình chạy vô thời hạn nên application credential là lựa chọn
đúng. Xem cách cấu hình ở [installation.vi.md](installation.vi.md) §5.2.

### Service catalog

Sau khi đăng nhập, Keystone trả về một **catalog**: bảng tra "tên dịch vụ →
URL". Nhờ đó Kuryr chỉ cần biết địa chỉ Keystone (`auth_url`), còn địa chỉ của
Neutron và Octavia thì tự tra ra. Đây cũng là lý do Kuryr biết đám mây **có**
Octavia hay không: nếu catalog không có mục load-balancer thì Service kiểu
`LoadBalancer` sẽ được báo là không hỗ trợ, thay vì lỗi mơ hồ lúc chạy.

---

## 4. Neutron — mạng

Đây là phần Kuryr dùng nhiều nhất. Có sáu khái niệm cần nắm.

### 4.1 Network

Một miền L2 — hình dung như một switch ảo. Bản thân nó **không có dải địa chỉ**;
nó chỉ là cái switch.

Thuộc tính đáng chú ý là **MTU**. Driver nested-vlan sẽ *từ chối* cắm interface
nếu MTU của mạng pod lớn hơn MTU đường uplink của node — vì khi đó gói nhỏ vẫn
đi, gói lớn biến mất, và triệu chứng là "TLS handshake treo nhưng ping vẫn được",
rất khó lần ra.

### 4.2 Subnet

Một dải CIDR gắn vào một network, kèm gateway, DNS và các route tĩnh. Một
network có thể mang nhiều subnet.

> **Tương đương Kubernetes:** đây chính là nơi pod CIDR đến từ đó. Chỉ khác là
> Neutron thật sự quản lý việc cấp phát — không có IPAM riêng của CNI plugin.

Kuryr cần **hai subnet** ở mức tối thiểu:

| Cấu hình | Dùng cho | Ghi chú |
| --- | --- | --- |
| `pod_subnet` | Địa chỉ của pod | Pod thật sự sống ở đây |
| `service_subnet` | ClusterIP của Service | **Không có gì sống ở đây.** Nó chỉ tồn tại để Octavia giữ được VIP tại đúng địa chỉ clusterIP mà Kubernetes đã cấp |

`service_subnet` phải trùng với `--service-cluster-ip-range` của cụm. Đây là
chỗ sai thường gặp nhất khi cài lần đầu.

### 4.3 Port

**Khái niệm trung tâm của Kuryr.** Một port là một card mạng ảo: có địa chỉ MAC,
một hay nhiều địa chỉ IP (`fixed_ips`), một tập security group, và một trạng thái.

Đời một port:

```
  tạo ─► DOWN ─► (Neutron lập trình dataplane) ─► ACTIVE
```

Kuryr **giữ lệnh CNI ADD chờ** cho tới khi port lên `ACTIVE`. Trả về sớm hơn
nghĩa là pod khởi động trên một interface mà luồng chuyển mạch chưa được lập
trình — biểu hiện ra ngoài là mất gói ngẫu nhiên chứ không phải một lỗi rõ ràng.

Vài trường quan trọng:

- **`device_owner`** — ai sở hữu port. Kuryr đóng dấu `compute:kuryr` lên mọi
  port của mình, nhờ đó dọn rác về sau biết chính xác cái gì là của mình.
- **`binding:host_id`** — port nằm ở máy nào. Đây là thứ khiến Neutron lập
  trình dataplane trước, thay vì chờ tới lúc pod chạy.
- **`binding:vif_type`** — Neutron *trả lời* nó đã cắm bằng cách nào: `ovs`,
  `bridge`… Giá trị `binding_failed` nghĩa là Neutron không cắm được.
- **`allowed_address_pairs`** — danh sách địa chỉ mà port này **cũng** được
  phép gửi đi. Mặc định Neutron chặn chống giả mạo: gói có source address
  không thuộc port sẽ bị vứt. Driver `nested-macvlan` phụ thuộc hoàn toàn vào
  trường này (xem §6.3).

### 4.4 Security group

Firewall có trạng thái, gắn vào **port** chứ không gắn vào máy. Một port có thể
mang nhiều group, và Neutron **hợp** các luật lại: một group mở thêm cổng thì
port đó được mở cổng ấy. Security group **chỉ có luật cho phép** — không có luật
cấm; cái gì không được cho phép thì bị chặn.

> **Tương đương Kubernetes:** NetworkPolicy. Kuryr dịch thẳng: mỗi
> NetworkPolicy thành một security group, mỗi rule thành các security group rule,
> rồi gắn group lên port của các pod được policy chọn.

Tính chất "hợp các luật" giải thích một điểm tinh tế trong thiết kế Kuryr:
NetworkPolicy của Kubernetes quyết định **theo từng pod** rằng chiều nào bị hạn
chế. Nếu không policy nào quản chiều egress của một pod, pod đó phải được mở
egress hoàn toàn — nhưng không thể mở bằng cách thêm luật vào group của một
policy nào đó, vì group ấy còn gắn lên các pod khác. Vì thế Kuryr dùng một
group "allow-all" dùng chung riêng cho chiều không bị quản.

### 4.5 Router

Nối các subnet với nhau và nối ra ngoài. Không có router thì một subnet là một
ốc đảo.

Chỉ cần tới khi bật `pod_subnets_driver = namespace` — lúc đó mỗi Kubernetes
namespace có subnet Neutron riêng, và tất cả phải cắm vào cùng một router
(`[namespace_subnet]pod_router`) thì các namespace mới nói chuyện được với nhau.

### 4.6 Floating IP

Một địa chỉ trên mạng ngoài, gắn động vào một port nội bộ (NAT 1-1).

> **Tương đương Kubernetes:** chính là cái mà `kubectl get svc` hiển thị ở cột
> `EXTERNAL-IP` cho Service kiểu `LoadBalancer`.

Kuryr phân biệt hai nguồn, vì nó quyết định lúc dọn dẹp:

- **lấy từ pool** (Kuryr tự cấp) → khi Service bị xoá thì **xoá** luôn;
- **do người dùng chỉ định** qua `spec.loadBalancerIP` → chỉ **gỡ liên kết**,
  không xoá, vì địa chỉ đó không phải của Kuryr.

### 4.7 Trunk và subport — dành cho cụm chạy trong VM

Đây là phần khó nhất, và chỉ liên quan khi **node Kubernetes bản thân nó là máy
ảo OpenStack** (gọi là *nested*, hay "VM trong VM").

Vấn đề: node là một VM, VM đó có đúng một port. Pod chạy bên trong node cần port
riêng — nhưng compute host chỉ thấy cái port của VM.

Giải pháp của Neutron là **trunk**:

```
   VM node  ── parent port  ──┬── subport (VLAN 101)  ──►  pod A
                              ├── subport (VLAN 102)  ──►  pod B
                              └── subport (VLAN 103)  ──►  pod C
```

- **Trunk** gắn vào port của VM node (*parent port*).
- Mỗi pod nhận một **subport** kèm một **VLAN id**.
- Bên trong node, Kuryr tạo interface con 802.1Q mang đúng VLAN id đó và đẩy
  vào network namespace của pod.
- Compute host tách VLAN ra và coi mỗi subport như một port độc lập.

Giới hạn: VLAN id có 12 bit, nên **tối đa 4094 pod trên một node**. Thực tế
không bao giờ chạm tới.

Trunk là một *extension* của Neutron — không phải đám mây nào cũng bật. Kuryr
dò danh sách extension lúc khởi động và báo ngay nếu thiếu, thay vì để hỏng lúc
tạo pod đầu tiên.

---

## 5. Octavia — cân bằng tải

Octavia hiện thực Kubernetes Service. Bốn lớp, lồng nhau:

```
  Load balancer   (giữ VIP = clusterIP của Service)
    └── Listener  (một cặp giao thức + cổng, ví dụ TCP:80)
          └── Pool      (thuật toán phân tải)
                └── Member   (một backend: IP + cổng)
```

Ánh xạ từ Kubernetes:

| Kubernetes | Octavia |
| --- | --- |
| Service | Load balancer |
| mỗi `spec.ports[]` | Listener + Pool |
| mỗi endpoint sẵn sàng | Member |
| `spec.clusterIP` | VIP của load balancer |
| `status.loadBalancer.ingress` | Floating IP |

### Provider và amphora

Octavia có nhiều **provider** (driver hiện thực):

- **amphora** — provider mặc định. Mỗi load balancer là **một máy ảo thật** chạy
  HAProxy. Chắc chắn nhưng tốn tài nguyên và **tạo mất hàng chục giây**.
- **ovn** — provider phân tán, cài luật thẳng vào OVN. Nhanh hơn nhiều, nhưng
  ít tính năng hơn.

Đây là lý do controller **không chờ đồng bộ** mà ghi lại ID rồi requeue: Octavia
chỉ nhận thay đổi khi load balancer đang ở trạng thái `ACTIVE`, còn trong lúc
`PENDING_CREATE` thì từ chối mọi thao tác.

Kuryr kiểm tra provider được cấu hình có tồn tại hay không **ngay lúc khởi
động**, vì nếu để Octavia tự báo thì lỗi chỉ hiện lúc tạo load balancer đầu
tiên và thông báo không nói rõ sai ở đâu.

### `member_mode`: L3 và L2

Cách load balancer nói chuyện với backend:

- **L3** (mặc định) — đi qua router. Octavia được báo subnet của chính load
  balancer. Đơn giản, chạy được ở mọi topology.
- **L2** — bắc cầu trực tiếp. Octavia cần biết subnet **thật** của từng backend,
  nên Kuryr phải tra subnet của từng pod, và `[pod_vif_nested]worker_nodes_subnets`
  trở thành bắt buộc cho các backend nằm trên mạng node.

### `enforce_sg_rules`

Với provider amphora, **amphora viết lại địa chỉ nguồn** của gói tin. Nghĩa là
security group của backend không còn nhìn thấy client thật — nó chỉ thấy
amphora. Nếu để nguyên, luật firewall của pod sẽ vô hiệu.

Bật `enforce_sg_rules` (mặc định) khiến Kuryr suy ra luật cho VIP từ những gì
backend cho phép, để việc lọc vẫn xảy ra — chỉ là xảy ra ở phía trước.

---

## 6. Từ Kubernetes sang OpenStack: ánh xạ đầy đủ

### 6.1 Bảng ánh xạ

| Kubernetes | OpenStack | Do thành phần nào tạo |
| --- | --- | --- |
| Pod | Neutron port | `kuryr-controller` |
| Namespace (chế độ `namespace`) | Network + subnet + router interface | `kuryr-controller` |
| NetworkPolicy | Security group + các rule | `kuryr-controller` |
| Service | Octavia load balancer + listener + pool | `kuryr-controller` |
| EndpointSlice | Octavia member | `kuryr-controller` |
| Service `LoadBalancer` | Floating IP | `kuryr-controller` |
| interface trong pod | veth / VLAN / macvlan | `kuryr-daemon` |

### 6.2 Bốn CRD ở giữa

Kuryr **không** gọi OpenStack trực tiếp từ các handler theo dõi Kubernetes.
Ở giữa luôn có một CRD, để trạng thái nhìn thấy được bằng `kubectl` chứ không
chỉ nằm trong log:

| CRD | Tên tắt | Chứa gì |
| --- | --- | --- |
| `KuryrPort` | `kp` | Các Neutron port của một pod |
| `KuryrNetwork` | `kns` | Network và subnet của một namespace |
| `KuryrNetworkPolicy` | `knp` | Một NetworkPolicy đã dịch thành security group rule |
| `KuryrLoadBalancer` | `klb` | Topology Octavia của một Service |

Khi pod kẹt ở `ContainerCreating`, lý do nằm ngay trên object:

```sh
kubectl describe kuryrport -n <namespace> <tên-pod>
```

### 6.3 Bốn cách cắm pod

Chọn bằng `[kubernetes]pod_vif_driver`:

| Driver | Node là gì | Cơ chế | Đánh đổi |
| --- | --- | --- | --- |
| `neutron-vif` | máy vật lý | veth, một đầu cắm vào integration bridge | Mặc định. Đơn giản nhất |
| `nested-vlan` | máy ảo | subport trên trunk + interface 802.1Q | Cần extension `trunk`. Tối đa 4094 pod/node |
| `nested-macvlan` | máy ảo | macvlan con của uplink + allowed address pair | Không cần trunk, không giới hạn số pod, nhưng **Neutron không lọc được traffic**. Không dùng được port pool |
| `nested-dpdk` | máy ảo | Nova gắn interface, pod tự lái bằng driver userspace | Cho workload NFV. Không dùng được port pool |

Với `nested-macvlan`, địa chỉ của pod **phải** được ghi vào
`allowed_address_pairs` của port VM node — nếu không, chống giả mạo của Neutron
vứt sạch gói pod gửi đi. Đây cũng là lý do driver này không dùng được port pool,
giống bản Python: port chỉ dùng được sau khi đã sửa một port dùng chung cho cả
node, nên không có gì để tạo sẵn.

### 6.4 Port pool

Tạo một Neutron port mất vài trăm mili giây tới vài giây. Nhân với số pod khởi
động cùng lúc khi một node vào cụm thì rất đáng kể.

Bật `vif_pool_driver = neutron` khiến Kuryr **tạo sẵn** port theo lô và giữ ấm.
Pod lấy port từ pool là lấy ngay; pod bị xoá thì trả port về pool thay vì xoá.

Đánh đổi: quota Neutron đổi lấy độ trễ khởi động pod. Mỗi tổ hợp
(node, project, network, tập subnet) có một pool riêng, và mỗi pool giữ ít nhất
`ports_pool_min` port.

---

## 7. Hai tiến trình và ranh giới giữa chúng

```
  ┌──────────────────┐   CRD    ┌──────────────────┐
  │ kuryr-controller │◄────────►│   kuryr-daemon   │
  │   (Deployment)   │  qua K8s │   (DaemonSet)    │
  └────────┬─────────┘  API     └────────┬─────────┘
           │                             │
           ▼                             ▼
      OpenStack API              netlink / OVS trên node
```

Ranh giới rất cứng, và đó là điều làm hệ thống dễ suy luận:

- **Controller không bao giờ chạm vào node.** Nó chỉ gọi hai API.
- **Daemon không bao giờ nói chuyện với OpenStack.** Nó chỉ đọc CRD và cắm dây.

Hệ quả thực tế: OpenStack sập thì các pod đang chạy **không bị ảnh hưởng** —
daemon vẫn phục vụ được; chỉ pod mới là không tạo được.

Ngoài ra còn hai binary nữa:

- **`kuryr-cni`** — plugin mà container runtime gọi. Nó không chứa logic mạng
  nào: chỉ chuyển tiếp môi trường CNI sang `kuryr-daemon` qua socket loopback.
- **`kuryr-status`** — kiểm tra trước khi cài: cấu hình, CRD, endpoint và
  extension mà các driver đã chọn cần tới.

---

## 8. Tự kiểm tra

Nếu trả lời được những câu này thì bạn đã đủ nền để đọc tiếp:

1. Tại sao `service_subnet` phải trùng service CIDR của cụm, trong khi không có
   gì sống trên subnet đó?
2. Tại sao CNI ADD phải chờ port lên `ACTIVE` mới trả về?
3. Cụm nested cần extension Neutron nào, và giới hạn số pod trên một node là
   bao nhiêu, vì sao?
4. Tại sao `nested-macvlan` không dùng được port pool?
5. Vì sao security group của một NetworkPolicy không thể tự chứa luật
   "mở hết egress"?

Đáp án nằm ở §4.2, §4.3, §4.7, §6.3 và §4.4 theo đúng thứ tự.

---

## Bảng thuật ngữ

| Tiếng Anh | Tiếng Việt | Ghi chú |
| --- | --- | --- |
| project / tenant | dự án | Đơn vị cô lập tài nguyên; xấp xỉ Namespace nhưng cứng hơn |
| domain | miền | Tầng trên project |
| application credential | thông tin xác thực ứng dụng | Nên dùng thay mật khẩu cho dịch vụ chạy dài |
| service catalog | danh bạ dịch vụ | Bảng tra tên dịch vụ → URL |
| network | mạng | Miền L2, không có dải địa chỉ |
| subnet | mạng con | Dải CIDR + gateway + DNS |
| port | cổng mạng | Card mạng ảo. **Không phải** cổng TCP |
| fixed IP | địa chỉ cố định | Địa chỉ Neutron cấp cho port |
| security group | nhóm bảo mật | Firewall có trạng thái, gắn vào port |
| allowed address pairs | cặp địa chỉ được phép | Địa chỉ mà port cũng được gửi đi |
| router | bộ định tuyến | Nối các subnet |
| floating IP | địa chỉ nổi | IP ngoài, NAT 1-1 vào port nội bộ |
| trunk | trunk | Gom nhiều subport lên một parent port |
| subport | subport | Port con trên trunk, mang một VLAN id |
| parent port | port cha | Port của VM node, nơi trunk gắn vào |
| integration bridge | cầu tích hợp | Bridge OVS trên node, mặc định `br-int` |
| load balancer | bộ cân bằng tải | Giữ VIP |
| listener | listener | Một cặp giao thức + cổng |
| pool | pool | Tập backend sau một listener |
| member | member | Một backend: IP + cổng |
| VIP | VIP | Địa chỉ ảo của load balancer |
| amphora | amphora | VM chạy HAProxy của provider mặc định |
| provider | provider | Driver hiện thực của Octavia |
| quota | hạn ngạch | Giới hạn tài nguyên theo project |
| extension | phần mở rộng | Tính năng tuỳ chọn của API Neutron |

---

## Đọc tiếp

| Tài liệu | Nội dung |
| --- | --- |
| [installation.vi.md](installation.vi.md) | Cài đặt từ đầu, tiếng Việt |
| [makefile.vi.md](makefile.vi.md) | Toàn bộ target của Makefile, tiếng Việt |
| [architecture.md](architecture.md) | Kiến trúc chi tiết, tiếng Anh |
| [configuration.md](configuration.md) | Từng khoá `kuryr.conf`, tiếng Anh |
