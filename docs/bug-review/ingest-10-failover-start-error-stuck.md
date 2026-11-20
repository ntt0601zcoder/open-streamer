# Lỗi đồng bộ khi Start input dự phòng làm stream kẹt mà không báo Exhausted
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, manager, ingestor/pull
- **Vị trí:** internal/ingestor/service.go:476-480; internal/ingestor/reader.go:116-142; internal/ingestor/pull/copy.go:86-89; internal/manager/service.go:880-882, 904-939, 808-824

## Hiện tượng
Input chính chết, input dự phòng không tạo được reader (upstream của `copy://` đã bị xoá, VOD mount bị gỡ). Stream ngừng phát vĩnh viễn nhưng trạng thái không phải Exhausted, không có hook, và không có retry. Chỉ có một dòng log `failed to start new ingestor`.

## Điều kiện kích hoạt
1. p0 là pull, p1 = `copy://other` hoặc `file://mount/x`.
2. Stream `other` bị xoá hoặc mount bị gỡ. `NewPacketReader` sẽ trả lỗi.
3. p0 gặp EOF hoặc lỗi fatal; worker thoát và gọi `ReportInputError`.

## Phân tích
`startPullWorker` trả lỗi ngay khi không dựng được reader, trước khi có worker. Trong manager:
```go
if err := s.ingestor.Start(ctx, streamID, bestInput, bufID); err != nil {
    slog.Error("manager: failed to start new ingestor", ...)
    return
}
```
p1 vẫn là Idle, `state.active` vẫn là 0, `exhausted` vẫn false. Từ đó không còn đường nào hoạt động:

- `collectTimeoutIfNeeded` bỏ qua vì p0 đã Degraded.
- `collectProbeIfNeeded` bỏ qua vì `priority == state.active && !state.exhausted`.
- `shouldFailbackNow` chỉ xét best có priority nhỏ hơn active.
- `ReportInputError` tiếp theo cho p0 không xảy ra vì worker đã thoát.

## Vì sao đây là lỗi
`Register` đã được sửa cho đúng trường hợp này (comment A-5 đưa lỗi vào đường degrade), nhưng `tryFailover` và `UpdateInputs` không làm vậy. Coordinator không nhận được tín hiệu degrade nên báo Active, và reconciler cũng bỏ qua.

## Hướng sửa
Khi `Start` lỗi, đánh Degraded input đó (ghi error history, `degradedAt`) rồi gọi lại `tryFailover`, để rơi về input khác hoặc vào Exhausted, từ đó probe tiếp tục. Tránh đệ quy vô hạn bằng cách lặp qua ứng viên.

## Cách xác minh
Test trong `internal/manager` với `ingestorDep` stub trả lỗi cho priority 1: `ReportInputError(0, io.EOF)`, chạy vài tick `checkHealth`, kiểm tra `exhausted` hiện vẫn false và không có probe nào. Thủ công: dùng `copy://` tới stream đã xoá làm dự phòng rồi dừng nguồn chính.
