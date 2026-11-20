# Hầu hết event gửi cho hook có id rỗng và occurred_at bằng năm 0001

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/events, internal/hooks, internal/coordinator, internal/api/handler
- **Vị trí:** internal/domain/event.go:83-87, internal/events/bus.go:65-74, internal/coordinator/coordinator.go:413-416, internal/coordinator/coordinator.go:536-539, internal/api/handler/stream.go:460-463, internal/hooks/batcher.go:384

## Hiện tượng
Webhook nhận payload kiểu `{"id":"","type":"stream.started","occurred_at":"0001-01-01T00:00:00Z",...}`. Người nhận không khử trùng lặp được khi batch bị gửi lại, không biết event xảy ra lúc nào. File hook cũng ghi như vậy.

## Điều kiện kích hoạt
Bất kỳ event nào không phải `session.opened/closed` hay test event.

## Phân tích
Các nơi phát chỉ điền `Type`, `StreamCode`:
```go
c.bus.Publish(ctx, domain.Event{
    Type:       domain.EventStreamStarted,
    StreamCode: stream.Code,
})
```
`Publish` đưa event thẳng vào queue, không gán gì:
```go
case b.queue <- event:
```
Grep toàn bộ `internal` chỉ thấy `OccurredAt`/`ID` được gán trong `sessions/tracker.go` và test event của hooks.

## Vì sao đây là lỗi
`domain.Event.ID` được ghi chú "UUID for idempotent delivery". Batcher cố ý gửi lại batch thất bại, và request lỗi timeout sau khi đích đã xử lý sẽ gây trùng lặp, nên `id` là bắt buộc để người nhận xử lý. Không có thời điểm thì batch trễ (tới 36 giây hoặc lâu hơn) không còn sắp xếp được.

## Hướng sửa
Trong `inProcessBus.Publish`, gán `uuid.NewString()` khi `ID == ""` và `time.Now()` khi `OccurredAt.IsZero()`, chạy trước khi đưa vào queue.

## Cách xác minh
Unit test: `Publish` event chỉ có `Type`, subscriber kiểm tra `ID` và `OccurredAt` hiện rỗng. Hoặc đăng ký hook file và đọc một dòng event `stream.started`.
