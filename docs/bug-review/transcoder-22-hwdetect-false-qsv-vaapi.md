# hwdetect báo QSV và VAAPI sai trên máy có CPU Intel và GPU NVIDIA

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/hwdetect, internal/api/handler
- **Vị trí:** internal/hwdetect/detect.go:34-39, internal/hwdetect/detect.go:52-73, internal/api/handler/config.go:142

## Hiện tượng
Trên server Intel Xeon với card NVIDIA, API trả `hw_accels` gồm cả `vaapi` và `qsv`. Chọn các giá trị này sẽ khiến encoder không mở được và transcoder respawn liên tục.

## Điều kiện kích hoạt
Linux có `/dev/dri/renderD*` (driver `nvidia-drm` cũng tạo node này) và có bất kỳ thiết bị PCI hãng Intel nào. Host bridge, NIC hay SATA của Intel đều tính.

## Phân tích
```go
if strings.TrimSpace(string(data)) == "0x8086" { return true }
```

`hasIntelGPU` chỉ so vendor mà không kiểm tra class của thiết bị. `hasDRIDevice` chỉ kiểm tra có render node, không xác định driver hay khả năng VA-API.

## Vì sao đây là lỗi
Tên hàm và mục đích là phát hiện GPU Intel, nhưng điều kiện đúng với gần như mọi máy dùng nền tảng Intel. Kết quả được đưa thẳng lên API làm danh sách backend khả dụng.

## Hướng sửa
Đọc thêm `/sys/bus/pci/devices/*/class` và chỉ chấp nhận class display (`0x03xxxx`) của Intel. Với VAAPI, xác định driver qua `/sys/class/drm/renderD*/device/driver` và loại `nvidia`.

## Cách xác minh
Trên máy Intel kèm NVIDIA, so sánh `GET /config/defaults` hoặc `hw_accels` với `lspci -nn | grep -E 'VGA|Display'`. Tách hàm đọc sysfs để test bằng thư mục giả.
