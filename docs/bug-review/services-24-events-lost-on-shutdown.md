# Event cuối cùng lúc shutdown không bao giờ tới hook

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/events, internal/sessions, internal/hooks, internal/runtime
- **Vị trí:** internal/runtime/manager.go:198, internal/events/bus.go:97-105, internal/sessions/reaper.go:96-109, internal/hooks/service.go:226-244

## Hiện tượng
Khi server dừng, hook phân tích không nhận `session.closed reason=shutdown` và mất các event còn trong queue. Bản ghi xem phim phía sau mãi không có điểm kết thúc.

## Điều kiện kích hoạt
SIGTERM khi có session đang hoạt động, hoặc còn event trong queue.

## Phân tích
Worker của bus dùng rootCtx và thoát ngay, không xả queue:
```go
case <-ctx.Done():
    return
```
Reaper dùng cùng gốc context và phát event đóng sau khi ctx đã huỷ:
```go
s.closeByID(id, domain.SessionCloseShutdown, 0)
```
Lúc đó `hooks.Service.Start` cũng đã unsubscribe tất cả và dừng batcher. Event được đưa vào queue không còn ai đọc, và `select` trong worker chọn ngẫu nhiên nên cũng có thể bỏ lại event đã xếp hàng trước đó.

## Vì sao đây là lỗi
Comment ở `shutdownActiveSessions` nêu rõ mục tiêu: "we still want EventSessionClosed to publish so persistence hooks complete". Thứ tự dừng hiện tại khiến việc đó không thể xảy ra.

## Hướng sửa
Dừng theo thứ tự: sessions, sau đó bus xả queue có giới hạn thời gian, rồi hooks drain. `main` cần chờ các bước này (hiện `WaitAll` không được gọi).

## Cách xác minh
Test: bus thật, subscriber, huỷ ctx, gọi `Publish` sau khi huỷ, kiểm tra handler không được gọi.
