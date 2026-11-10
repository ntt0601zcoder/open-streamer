# Worker cũ trong pha handoff ghi đè trạng thái input đã bị chuyển đi

- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** internal/manager, internal/ingestor
- **Vị trí:** internal/manager/service.go:607-622, internal/manager/service.go:941-949, internal/manager/service.go:988-991, internal/ingestor/service.go:577-585, internal/ingestor/worker.go:509-512

## Hiện tượng
Sau failover hoặc failback, API hiển thị hai input cùng `active` và gauge `manager_input_health=1` cho input đã bỏ. Input đang Degraded có thể nhảy về Active do vài packet muộn, và manager failback vào nguồn chập chờn mà không qua kiểm tra probe.

## Điều kiện kích hoạt
1. `tryFailover` chuyển từ A sang B. `commitSwitch` chạy ngay khi `ingestor.Start` return, trước khi B Open xong.
2. Worker A vẫn sống tới khi B handoff (cố ý, để tránh gap) và vẫn gửi packet.

## Phân tích
`RecordPacket` không xét input có đang active hay không:
```go
if h.Status != domain.StatusActive {
    h.Status = domain.StatusActive
    h.Errors = nil
    delete(state.degradedAt, inputPriority)
    s.m.ManagerInputHealth.WithLabelValues(...).Set(1)
```
Input không active không bao giờ bị timeout (`collectTimeoutIfNeeded` chỉ xét active), nên trạng thái Active này giữ nguyên sau khi worker A đã bị cancel. Gauge đặt về 0 ở `tryFailover` bị viết đè.

Với A đang Degraded và có blip, `selectBest` coi A khoẻ, và `shouldFailbackNow` kích hoạt sau cooldown. Ngưỡng `minProbeSpan` và `minProbePackets` được thêm để chặn đúng kiểu flapping này nhưng bị bỏ qua.

## Vì sao đây là lỗi
Comment trong `tryFailover` nói muốn tránh việc backup vẫn báo healthy sau khi chuyển. Lưu ý: các đường push hợp lệ (pusher quay lại) cần tiếp tục đưa input về Active, nên việc phân biệt cần tính đến loại input.

## Hướng sửa
Truyền worker generation vào callback, bỏ qua packet của worker đã thay thế. Với input pull không active, không nâng Status; chỉ probe mới được đổi trạng thái.

## Cách xác minh
Test manager: sau khi switch từ 0 sang 1 (`active=1`), gọi `RecordPacket(0)` và khẳng định trạng thái input 0 không phải Active với input pull.
