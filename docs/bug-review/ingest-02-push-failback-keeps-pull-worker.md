# Chuyển sang input publish:// không dừng pull worker, hai nguồn cùng ghi vào một buffer
- **Mức độ:** Nghiêm trọng
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor, ingestor/push, manager, buffer
- **Vị trí:** internal/ingestor/service.go:352-361, 619-660; internal/ingestor/push/rtmp_server.go:581-607; internal/manager/service.go:589-650, 808-824, 932

## Hiện tượng
Với cấu hình hay gặp là `publish://` làm input chính và một nguồn pull (HLS, file bảo trì, RTMP) làm dự phòng, khi encoder kết nối thì hình ảnh của encoder và của nguồn dự phòng bị trộn xen kẽ trong cùng buffer. Người xem thấy giật, nhảy hình, lỗi giải mã, PTS đan xen. Tình trạng kéo dài tới khi stream restart. Chiều ngược lại cũng xảy ra: pull chính chuyển sang publish dự phòng thì pull worker vẫn chạy và tiếp tục ghi khi nguồn sống lại.

## Điều kiện kích hoạt
1. Stream có p0 = `publish://`, p1 = `https://backup/index.m3u8`.
2. Lúc khởi động chưa có encoder: `ErrNoPusherConnected` đẩy p0 sang Degraded, manager `Start(p1)` và worker W1 chạy.
3. Encoder đẩy vào. `OnReadRtmpAvMsg` ghi thẳng vào buffer ngay, trong khi W1 vẫn ghi.
4. Sau cooldown 12 s, `shouldFailbackNow` chọn p0 và gọi `Start(p0)`.

## Phân tích
`Start` rẽ nhánh theo URL. Nhánh push không chạm `s.workers`:
```go
if protocol.IsPushListen(input.URL) {
    return s.startPushRegistration(streamID, input, bufferWriteID)
}
```
Không có đoạn nào trong `startPushRegistration` huỷ worker hiện tại; `s.workers` chỉ được huỷ trong `Stop` và `startPullWorker`. Sau failback, `commitSwitch` đặt active=0 nhưng W1 vẫn sống, gắn với `monCtx`.

Từ bước 3, `RecordPacket` của p0 và p1 đều cập nhật `Active`, nên không có tín hiệu lỗi. `Register` ở `startPushRegistration` còn xoá cờ `active` khi encoder đang kết nối, và `errObserver(..., ErrNoPusherConnected)` được báo dù có pusher, đánh Degraded nhầm p0 trong chốc lát.

Ngược lại, khi chuyển từ publish sang pull do timeout, `pubState` không bị đóng nên encoder hồi phục lại ghi đồng thời với pull worker.

## Vì sao đây là lỗi
Comment của `Buffer.Write` nêu rõ "Only the active Ingestor goroutine for this stream should call Write", và invariant là manager thay input bằng cách đổi ingestor. Handoff chỉ tồn tại trong `startPullWorker`, cho trường hợp pull thay pull. Không có guard ở buffer vì hub không biết ai là writer hợp lệ. `service_test.go` chỉ kiểm tra `pushStreamKey`, không có test pull↔push.

## Hướng sửa
Trong `startPushRegistration`, huỷ và xoá `s.workers[streamID]` (có thể đợi tới khi có pusher thực). Khi chuyển khỏi push, chặn ghi từ `pubState` không còn là input active, hoặc dispose session. Chỉ báo `ErrNoPusherConnected` khi slot thực sự trống.

## Cách xác minh
Test với `buffer.NewServiceForTesting` và reader giả: `Start` với input pull, rồi `Start` với `publish://`; kiểm tra context của worker pull đã bị huỷ. Thủ công: bật dự phòng HLS, đẩy RTMP, dùng `ffprobe` đọc output xem có PTS của hai nguồn đan xen.
