# Update không kiểm tra lại trạng thái chạy, PUT trong lúc Stop tạo pipeline mồ côi

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/coordinator, internal/publisher, internal/transcoder, internal/dvr/blob, internal/manager
- **Vị trí:** internal/api/handler/stream.go:404, internal/api/handler/stream.go:447-451, internal/api/handler/template.go:253-258, internal/api/handler/config_yaml.go:596-597, internal/coordinator/coordinator.go:545-636, internal/coordinator/coordinator.go:640-707, internal/coordinator/coordinator.go:373-383

## Hiện tượng
Sau một `PUT` chạy đua với `DELETE`, disable, restart hoặc reaper: subprocess transcoder, publisher hoặc DVR vẫn chạy cho một stream không có ingest. Stream có thể không còn trong store nhưng vẫn tốn CPU/GPU. Với restart, `Start` tiếp theo báo `publisher: stream X already running` và stream không lên được.

## Điều kiện kích hoạt
1. `PUT` đổi transcoder: `wasRunning=true`, rồi `Update` chờ lifecycle lock.
2. Trong lúc đó `DELETE` hoặc reaper đang giữ lock và chạy `stopLocked` (mất từ vài chục mili giây tới vài giây do DVR, publisher, subprocess).
3. Stop xong, `updateLocked` chạy trên stream đã dừng.

## Phân tích
`IsRunning` được kiểm tra trước khi lấy lock, còn `startLocked` có re-check dưới lock nhưng `updateLocked` thì không. `reloadTranscoderFull` chạy tiếp: tạo buffer, `UpdateBufferWriteID` no-op vì manager không còn đăng ký, rồi:
```go
c.tc.Start(ctx, new.Code, newIngestID, tcRT, transcoderTargets)
c.pub.Start(ctx, new)
c.reloadDVR(ctx, new)
```
Manager không đăng ký nên `IsRunning` là false, không ai trông coi pipeline này. Nhánh ABR copy/mixer còn gọi `startLocked` đầy đủ nên có thể khởi động lại stream đã xoá. Chỉ đổi DVR cũng đi tới `StartRecording`.

Trường hợp restart: `Start` thấy `IsRunning=false`, `mgr.Register` ok, `pub.Start` lỗi và rollback xoá buffer mà transcoder mồ côi đang ghi. Reconciler lặp lỗi này mỗi 10 giây.

## Vì sao đây là lỗi
Lifecycle lock chỉ tuần tự hoá các thao tác, không đảm bảo tiền đề `IsRunning` còn đúng. Comment C-1 chỉ nhắc Start và Stop.

## Hướng sửa
Re-check dưới lock ở đầu `updateLocked`: nếu stream không chạy thì return (hoặc so với store hiện tại). Các caller coi đây là no-op để reconciler xử lý.

## Cách xác minh
Test coordinator với spy: chặn `tc.Stop` hoặc `pub.Stop` bằng channel, gọi `Stop` trong goroutine, gọi `Update` đổi ladder, thả channel và khẳng định `tc.Start`/`pub.Start` không được gọi.
