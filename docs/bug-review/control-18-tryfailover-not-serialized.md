# tryFailover không được tuần tự hoá, ingestor.Start có thể lệch với state.active
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** manager, ingestor
- **Vị trí:** internal/manager/service.go:892, 899-941, 951-956, 982-987, 464-491, 779-785, 1109-1110; internal/ingestor/service.go:495-515, 600-603

## Hiện tượng
Hiếm gặp:
- Failback làm worker bị khởi động hai lần liên tiếp, gây thêm một lần reconnect, và metric `failovers_total` cùng event `input.failover` bị tính hai lần cho một lần chuyển.
- Worker đang chạy một input trong khi `active_input_priority` báo input khác, tự sửa sau khi hết packet timeout.
- Khi restart đua với failover, worker mới bị huỷ, stream mất dữ liệu tới hết packet timeout.

## Điều kiện kích hoạt
1. `runProbe` thành công và `checkHealth` chạy sweeper gần như cùng lúc: cả hai gọi `tryFailover(Failback)`.
2. `SwitchInput` đua với failover từ monitor, chọn input khác nhau.
3. `tryFailover` đã qua kiểm tra `dead` thì `Unregister` và `Register` mới chạy (`POST /restart`).

## Phân tích
`tryFailover` thả `state.mu` trước `ingestor.Start`, rồi mới commit:
```go
if state.dead || state.active != prevPriority {
    return
}
```
Khi `commitSwitch` bỏ qua vì active đã đổi, worker của lần `Start` đó vẫn đang chạy, và phần metric, event phía sau vẫn thực hiện vì `commitSwitch` không trả kết quả.

Với trường hợp 3, `Start` dùng `monCtx` cũ đã huỷ, nhưng vẫn thay entry trong `workers`. Nó lấy worker mới làm `prevCancel`. Worker mới bị huỷ ngay nếu cùng priority, hoặc khi goroutine cũ thoát nếu khác priority, trong khi worker thay thế đã chết theo context.

## Vì sao đây là lỗi
Comment của hàm ghi "safe to call concurrently; concurrent calls are idempotent", nhưng hai lần gọi đều chạy `ingestor.Start`. Kiểm tra `dead` và `active` chỉ xảy ra trước và sau khoảng không giữ lock.

## Hướng sửa
Thêm một mutex hoặc cờ `switching` theo stream bao quanh Start và commit, kiểm tra `dead` lại trước `Start`, và cho `commitSwitch` trả về bool để chỉ phát event khi commit thành công.

## Cách xác minh
Test với stub `Start` chặn bằng channel: chạy hai `tryFailover` song song cho cùng target, kiểm tra số lần `Start` và số lần tăng `ManagerFailoversTotal`. Hiện tại đều là 2.
