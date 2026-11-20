# Trạng thái push `failed` bị xóa ngay nên API và metric không bao giờ thấy
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP push, runtime status)
- **Vị trí:** internal/publisher/push_rtmp.go:392, 405-410; internal/publisher/runtime.go:24-25, 196-209, 214-244

## Hiện tượng
Khi một đích push hết số lần thử, đích đó biến mất khỏi `runtime.publisher.pushes[]` thay vì hiện `failed` kèm lỗi cuối. Gauge `open_streamer_publisher_push_state` cũng mất series. Người vận hành chỉ thấy đích đã biến mất, không biết lý do.

## Điều kiện kích hoạt
`dest.Limit > 0` và số lần thử đạt giới hạn.

## Phân tích
```go
defer s.removePushState(streamID, dest.URL)
...
if dest.Limit > 0 && attempts >= dest.Limit {
    s.setPushStatus(streamID, dest.URL, PushStatusFailed)
    return
}
```
`return` chạy ngay deferred `removePushState`, xóa state và `DeleteLabelValues` gauge. Lịch sử 5 lỗi gần nhất mất theo.

## Vì sao đây là lỗi
ARCHITECTURE và `runtime.go` mô tả `failed` là trạng thái quan sát được qua `runtime.publisher.pushes[]`, và gauge có giá trị riêng `0=failed`. Thực tế trạng thái chỉ tồn tại vài micro giây; chỉ có event `push.failed` là đến được bên ngoài.

## Hướng sửa
Không xóa state khi kết thúc do `failed`. Chỉ xóa khi context bị hủy (đích bị gỡ hoặc stream dừng).

## Cách xác minh
Test `serveRTMPPush` với URL không kết nối được, `Limit=1`, rồi kiểm tra `RuntimeStatus` có `failed` và lỗi.
