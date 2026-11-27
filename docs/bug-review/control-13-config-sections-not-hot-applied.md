# POST /config chấp nhận buffer, publisher, ingestor, watermarks nhưng không áp dụng
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** runtime, publisher, buffer, ingestor, watermarks, cmd/server
- **Vị trí:** internal/runtime/manager.go:231-370, 284-291; cmd/server/main.go:234-246; internal/ingestor/service.go:61, 108; internal/publisher/service.go:99, 172; internal/buffer/service.go:121, 130-132; internal/watermarks/service.go:75; docs/CONFIG.md:8-11, 49

## Hiện tượng
Operator đổi `publisher.hls.live_segment_sec`, `publisher.max_playback_conn_total`, `buffer.capacity`, `watermarks.dir` hoặc `hooks.worker_count`. API trả 200, `GET /config` hiện giá trị mới, nhưng server vẫn chạy với giá trị lúc boot, kể cả với stream start sau đó. Sau khi restart thì mới có tác dụng, nên operator dễ gặp thay đổi hành vi bất ngờ ở lần restart sau.

Riêng `ingestor.hls_max_segment_buffer`: thay đổi làm listener RTMP restart, ngắt kết nối mới, nhưng giá trị mới vẫn không được dùng.

## Điều kiện kích hoạt
`POST /config` hoặc `PUT /config/yaml` đổi một trong các trường trên.

## Phân tích
Các sub-config chỉ được đưa vào DI một lần lúc boot, và mỗi service giữ bản sao lúc khởi tạo. `Publisher.cfg`, `Buffer.cfg`, `Ingestor.cfg` không có setter. `runtime.Manager.diff` không có nhánh cho `Buffer`, `Publisher`, `Watermarks`. Với ingestor:
```go
ingChanged := configChanged(old.Ingestor, new.Ingestor) || ...
m.diffService("ingestor", wasIng, nowIng, ingChanged, ...)
```
`Run` chỉ đọc `currentListeners()`, không đọc lại `IngestorConfig`, nên restart không mang lại giá trị mới.

Publisher còn được kiểm tra `HLS.Dir` và `DASH.Dir` bằng giá trị cũ khi `Start`.

## Vì sao đây là lỗi
CONFIG.md ghi GlobalConfig "Hot-applied without restart", và CLAUDE.md nói `internal/runtime` diff rồi cycle các service bị ảnh hưởng. API không trả dấu hiệu nào là cần restart.

## Hướng sửa
Với từng section, chọn một trong hai: thêm setter và áp dụng cho stream start sau đó (buffer capacity, publisher), hoặc trả cảnh báo `restart_required` trong response và ghi rõ trong CONFIG.md. Bỏ `configChanged(old.Ingestor, ...)` khỏi điều kiện restart nếu giá trị đó không được đọc lại.

## Cách xác minh
Đọc code: `grep` setter trong các service trên. Trên môi trường thử: đổi `publisher.hls.live_window`, restart một stream qua API, đọc playlist. Số segment vẫn theo giá trị cũ.
