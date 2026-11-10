# Start từ snapshot cũ hồi sinh stream đã xoá hoặc đã disable

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/coordinator, internal/store
- **Vị trí:** internal/api/handler/stream.go:724-725, internal/api/handler/stream.go:750, internal/api/handler/stream.go:780-782, internal/api/handler/config_yaml.go:554-555, internal/coordinator/coordinator.go:941-966, internal/coordinator/coordinator.go:313-319

## Hiện tượng
Stream đã `DELETE` hoặc đã `disabled=true` vẫn chạy tiếp: vẫn ingest, transcode, push ra ngoài. Stream đã xoá không hiện trong `GET /streams` nên operator không thấy. Một biến thể khác: sau restart, stream chạy cấu hình cũ trong khi store đã là cấu hình mới.

## Điều kiện kích hoạt
Một trong các interleaving:
1. `DELETE`: `Stop` xong `Unregister` (`IsRunning=false`), reconciler tick thấy stream vẫn trong repo và gọi `Start`, handler mới `streamRepo.Delete`.
2. Reconciler có danh sách từ đầu vòng lặp, `Start` của các stream trước chiếm thời gian; `DELETE` hoặc PUT disable hoàn tất trước khi tới lượt stream đó.
3. `Restart` đọc `stream`, chạy `Stop` (vài giây); `DELETE`, disable hoặc PUT lọt vào lúc này (`wasRunning=false` nên PUT không áp), rồi `Start(stream cũ)`.

## Phân tích
Xoá pipeline trước, xoá store sau:
```go
h.coordinator.Stop(pipelineCtx, code)
if err := h.streamRepo.Delete(r.Context(), code); err != nil {
```
`startLocked` chỉ xét `IsRunning` và `stream.Disabled` của con trỏ do caller cung cấp, không đọc lại store. Reconciler chỉ khởi động, không bao giờ dừng stream không còn hợp lệ, nên trạng thái sai tồn tại vĩnh viễn.

## Vì sao đây là lỗi
Comment của `RunReconciler` nói race là an toàn vì "if the operator wants the stream stopped, they should toggle Disabled", nhưng snapshot cũ bỏ qua chính việc toggle đó và DELETE.

## Hướng sửa
Trong `DELETE`, xoá store trước rồi mới Stop. Khi reconciler và Restart bắt đầu Start, đọc lại bản ghi dưới lifecycle lock (cho phép qua callback để runtime stream không bị ảnh hưởng). Có thể cho reconciler dừng pipeline không còn bản ghi.

## Cách xác minh
Test coordinator với repo giả: `List` trả stream, trước khi `Start` chạy thì xoá khỏi repo, khẳng định pipeline không được tạo. Với handler: chặn `Stop` và chạy `reconcileOnce` đồng thời.
