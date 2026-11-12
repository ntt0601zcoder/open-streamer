# Push vào code của stream cấu hình pull làm auto-publish chiếm stream đó

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/ingestor/push, internal/autopublish, internal/coordinator, internal/api
- **Vị trí:** internal/ingestor/push/rtmp_server.go:303-329, internal/autopublish/service.go:150-211, internal/autopublish/service.go:312-325, internal/coordinator/coordinator.go:313-316, internal/api/handler/stream.go:715-722

## Hiện tượng
Một lần push (bị từ chối) vào path trùng stream pull đang chạy, thuộc prefix của template:
- `GET /streams` tag stream đó `source=runtime`.
- `DELETE /streams/<code>` trả 204 nhưng bản ghi vẫn còn, reconciler khởi động lại.
- Reaper có thể dừng stream cấu hình theo chu kỳ; reconciler bật lại, tạo gián đoạn lặp lại.

Nếu stream cấu hình đang dừng hoặc disabled, push còn tạo runtime stream theo cấu hình template dưới code đó.

## Điều kiện kích hoạt
1. Stream `region/north/foo` input pull; template có prefix `region` và input `publish://`.
2. Có encoder push `rtmp://host/region/north/foo`.

## Phân tích
Stream pull không có slot registry nên `Lookup` miss và gọi resolver. `ResolveOrCreate` không kiểm tra store:
```go
if err := s.coord.Start(ctx, resolved); err != nil {
```
`Start` là no-op vì `IsRunning=true`, nên service coi như thành công và ghi `entries[code]`. `registry.Acquire` sau đó lỗi (publisher bị từ chối), nhưng entry đã tồn tại và không được gỡ. Observer theo dõi buffer của stream cấu hình; nếu nó có transcoder (flow-lifecycle-04) hoặc nguồn im 30 giây thì reaper gọi `coord.Stop`.

`Delete` đi nhánh runtime trước:
```go
if h.autopublish != nil && h.autopublish.StopRuntime(pipelineCtx, code) {
```
nên bỏ qua `streamRepo.Delete`.

## Vì sao đây là lỗi
Tài liệu xác định runtime stream là code không có bản ghi cấu hình (`PUT` đã chặn hướng ngược lại với `RUNTIME_STREAM_NOT_EDITABLE`), nhưng `ResolveOrCreate` không chặn hướng này.

## Hướng sửa
Trong `ResolveOrCreate`, từ chối khi code đã có trong store hoặc khi `coord.IsRunning(code)` là true trước `Start`. Nếu `Acquire` sau đó thất bại, dọn entry vừa tạo.

## Cách xác minh
Unit test autopublish: coordinator giả báo `IsRunning=true`, gọi `ResolveOrCreate` và khẳng định lỗi, không tạo entry, `IsRuntime` false.
