# Hook không bao giờ nhận được nhiều loại event đã được công bố

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/hooks, internal/events, internal/api/handler, internal/autopublish, internal/publisher, internal/manager
- **Vị trí:** internal/hooks/service.go:203-214, internal/events/bus.go:77, internal/domain/event.go:13-78, internal/api/handler/stream.go:471-479, internal/api/handler/config.go:238-243, internal/autopublish/service.go:215, internal/publisher/runtime.go:141-143, internal/manager/service.go:1091, docs/EVENTS.md:41, docs/EVENTS.md:93-96, docs/EVENTS.md:111, docs/EVENTS.md:147-149, docs/EVENTS.md:211, docs/EVENTS.md:223, docs/EVENTS.md:233

## Hiện tượng
Hook khai báo `event_types: ["push.failed"]` hay `["stream.updated","config.changed"]`, đúng như ví dụ trong docs/EVENTS.md, không bao giờ được gọi. Không có lỗi hay log nào, trong khi `POST /hooks/{id}/test` vẫn thành công.

## Điều kiện kích hoạt
1. Tạo hook HTTP với `event_types: ["push.failed"]`.
2. Cho một đích push hết `limit`, hoặc lưu `POST /config`, sửa stream, sửa template.

## Phân tích
Bus chỉ giao event theo đúng loại đã `Subscribe`, không có wildcard. `hooks.Service.Start` đăng ký một danh sách cứng:

```go
allEvents := []domain.EventType{
    domain.EventStreamCreated, domain.EventStreamStarted,
    domain.EventStreamStopped, domain.EventStreamDeleted,
    ...
```

Danh sách thiếu: `stream.updated`, `stream.runtime_created`, `stream.runtime_expired`, `input.recovered`, `push.started`, `push.active`, `push.reconnecting`, `push.failed`, `config.changed`, `watermark.asset_created`, `watermark.asset_deleted`, `hook.*`, `template.*`, `policy.*`, `dvr.segment_pruned`. Các loại này vẫn được publish từ handler, autopublish, publisher và manager, nhưng không có subscriber.

## Vì sao đây là lỗi
Comment trong `UpdateConfig` nói event này dành cho operator theo dõi thay đổi cấu hình. docs/EVENTS.md có sẵn ví dụ hook dùng đúng các loại trên. `DeliverTestEvent` đi thẳng vào batcher, nên test hook không phát hiện được.

## Hướng sửa
Khai báo một danh sách đầy đủ các `EventType` trong `domain` và cho hooks duyệt danh sách đó, kèm test so với mọi hằng số. Hoặc thêm `SubscribeAll` vào bus.

## Cách xác minh
Test với bus thật: publish `EventPushFailed` và `EventConfigChanged`, kiểm tra hook có `event_types` tương ứng nhận được.
