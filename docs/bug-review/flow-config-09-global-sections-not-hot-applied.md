# Nhiều mục GlobalConfig được lưu và báo thành công nhưng không áp dụng khi chạy

- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/runtime, internal/api/handler, internal/api, internal/publisher, internal/buffer, internal/ingestor, internal/watermarks, cmd/server
- **Vị trí:** internal/runtime/manager.go:231-370, internal/runtime/manager.go:286-291, cmd/server/main.go:234-246, cmd/server/main.go:262-270, internal/publisher/service.go:99, internal/publisher/service.go:172-175, internal/buffer/service.go:130-133, internal/ingestor/service.go:61, internal/ingestor/service.go:100-108, internal/api/server.go:80-89, docs/CONFIG.md:11

## Hiện tượng
Sau `POST /config` đổi `publisher.hls.live_segment_sec`, `publisher.hls.dir`, `publisher.max_playback_conn_total`, `buffer.capacity`, `watermarks.dir`, `hooks.worker_count` hoặc `ingestor.hls_max_segment_buffer`: API trả 200, `GET /config` hiện giá trị mới, nhưng hành vi không đổi tới khi restart process. Đổi riêng `ingestor.*` còn dựng lại listener RTMP một cách vô ích.

## Điều kiện kích hoạt
`POST /config` với `{"publisher":{"hls":{"live_segment_sec":6}}}` rồi restart một stream: segment vẫn 2 giây.

## Phân tích
Các mục này chỉ đi vào service qua DI lúc khởi động (`provideSubConfigs`). `Service.cfg` của publisher, ingestor và buffer không có setter. `api.Server` cũng chụp `hlsDir`, `dashDir` một lần trong `New`. `runtime.Manager.diff` không có nhánh nào cho `old.Publisher`, `old.Buffer`, `old.Watermarks`.

Với ingestor, diff có xét nhưng cấu hình mới không tới nơi:

```go
ingChanged := configChanged(old.Ingestor, new.Ingestor) || ...
m.diffService("ingestor", wasIng, nowIng, ingChanged, func(ctx) error { return m.deps.Ingestor.Run(ctx) })
```

`Run` không đọc `IngestorConfig` mới, chỉ dựng lại RTMP server. Bản thân việc dựng lại đó gây lỗi nghiêm trọng đã được ghi ở `flow-lifecycle-07-rtmp-restart-loses-handlers.md`.

Hệ quả thêm khi đổi `publisher.hls.dir`: sau restart, bản cũ để lại thư mục cũ không được dọn.

## Vì sao đây là lỗi
docs/CONFIG.md dòng 11 ghi các mục này "Hot-applied without restart". Với các mục trên, thay đổi chỉ có tác dụng sau khi restart mà API không báo gì.

## Hướng sửa
Thêm setter nguyên tử cho các giá trị đọc khi khởi động stream, tương tự `SetListeners`. Với mục thực sự cần restart, trả cờ `restart_required` trong response và ghi rõ trong tài liệu.

## Cách xác minh
Test `runtime.Manager.diff` với `Publisher` khác nhau và kiểm tra có setter được gọi. Thử thực tế như ở phần điều kiện kích hoạt.
