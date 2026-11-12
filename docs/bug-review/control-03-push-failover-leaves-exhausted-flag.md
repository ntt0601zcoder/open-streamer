# Failover sang input publish:// chưa có encoder làm stream kẹt Degraded vĩnh viễn
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** manager, ingestor, coordinator
- **Vị trí:** internal/manager/service.go:919, 932, 941, 957, 962-977, 995, 694-707; internal/ingestor/service.go:654-658; internal/coordinator/coordinator.go:798-818

## Hiện tượng
Sau khi đã có hình trở lại và `runtime.exhausted=false`, `status` của stream vẫn là `degraded` và không tự về `active` cho tới khi restart stream.

## Điều kiện kích hoạt
1. Stream có inputs `[0: nguồn pull, 1: publish://...]`, encoder chưa kết nối.
2. Input 0 lỗi, manager failover sang input 1.
3. Sau đó encoder kết nối, hoặc input 0 hồi phục và failback.

## Phân tích
`startPushRegistration` gọi observer lỗi một cách đồng bộ, ngay bên trong `ingestor.Start` của `tryFailover` ngoài:
```go
if errObserver != nil {
    errObserver(streamID, input.Priority, ErrNoPusherConnected)
}
```
Trình tự:
1. `tryFailover` ngoài ghi `wasExhausted=false` rồi gọi `ingestor.Start(input 1)`.
2. `ReportInputError(1)` đánh dấu input 1 Degraded (có 2 input nên không vào nhánh bỏ qua). `tryFailover` trong thấy `best == nil`, gọi `handleExhausted`: `exhausted=true` và `go cb` đặt `inputsExhausted=true` ở coordinator.
3. `Start` trả nil. `commitSwitch` đặt `state.exhausted = false`.
4. `notifyRestored(wasExhausted=false, ...)` không gọi callback.

Từ đó manager có `exhausted=false` còn coordinator giữ `inputsExhausted=true`. `RecordPacket` chỉ gọi restored khi `state.exhausted` true, và `runProbe` cũng dựa vào cờ này, nên không còn đường nào xoá cờ của coordinator.

## Vì sao đây là lỗi
Comment ở `ReportInputError` đã nói tới rủi ro kẹt Degraded, nhưng chỉ chặn cho stream một input. Giá trị `wasExhausted` được đọc trước `ingestor.Start` nên không thấy cờ vừa được đặt trong lúc Start chạy, còn `commitSwitch` xoá cờ mà không báo.

## Hướng sửa
Đọc giá trị `exhausted` cũ ngay trong `commitSwitch` dưới `state.mu` và dùng nó cho `notifyRestored`. Tốt hơn nữa là không gọi observer đồng bộ trong `Start`, và không commit switch sang input vừa bị Degraded trong chính lần Start đó.

## Cách xác minh
Test với stub ingestor có `Start` gọi `ReportInputError(ErrNoPusherConnected)` cho priority 1. Kiểm tra callback exhausted được gọi, sau đó `exhausted=false` nhưng callback restored không được gọi. Trên server: tạo stream như trên, dừng nguồn 0, xem `status` sau khi encoder kết nối.
