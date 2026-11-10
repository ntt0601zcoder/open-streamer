# Series metric và lock theo stream tăng không giới hạn sau mỗi vòng đời

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/coordinator, internal/manager, internal/ingestor, internal/transcoder, internal/publisher, internal/autopublish, internal/buffer
- **Vị trí:** internal/coordinator/coordinator.go:198-208, internal/coordinator/coordinator.go:529-531, internal/manager/service.go:486-488, internal/manager/service.go:1179-1187, internal/ingestor/service.go:573-576, internal/transcoder/service.go:420-421, internal/publisher/service.go:641-679, internal/buffer/service.go:196-203, internal/buffer/service.go:282-286

## Hiện tượng
`/metrics` phình dần khi có nhiều stream vòng đời ngắn, nhất là auto-publish với path do encoder quyết định. Gauge như `transcoder_workers_active=0` vẫn được báo cho stream đã xoá. Bộ nhớ Prometheus và kích thước scrape tăng.

## Điều kiện kích hoạt
Lặp lại việc tạo và dừng stream với code khác nhau, hoặc xoá input khỏi stream đang chạy.

## Phân tích
Khi dừng stream chỉ xoá `StreamStartTimeSeconds`, `ManagerInputHealth`, nhóm buffer, push state/reconnect và vài gauge process. Các vector theo `stream_code` không có `DeleteLabelValues`: `IngestorBytesTotal`, `IngestorPacketsTotal`, `IngestorErrorsTotal`, `ManagerFailoversTotal`, `ManagerProbeAttemptsTotal`, `TranscoderWorkersActive`, `TranscoderQualitiesActive`, `TranscoderRestartsTotal`, `TranscoderFramesTotal`, `TranscoderErrorsTotal`, `PublisherSegmentsTotal`, `PublisherSegmentWriteDuration`, `PublisherPushBytes`.

Các nguồn khác:
- `UpdateInputs` xoá input nhưng giữ `ManagerInputHealth` của priority đó; `Unregister` chỉ xoá priority còn trong map.
- `sampleOnce` chụp danh sách rồi `Set` ngoài lock; nếu `Delete` chen vào, series vừa xoá được tạo lại và không còn ai xoá.
- `lifecycleLocks` không bao giờ bị prune.

## Vì sao đây là lỗi
Comment trong `Buffer.Delete` xác nhận ý định dọn series cho "short-lived auto-publish runtime", nhưng chỉ áp dụng cho buffer. Với auto-publish, nhãn đến từ path mà client điều khiển.

## Hướng sửa
Thêm bước dọn series theo stream khi `stopLocked` kết thúc (mỗi service một hàm cleanup). Prune `lifecycleLocks` an toàn. Kiểm tra buffer còn tồn tại trước khi `Set` trong sampler.

## Cách xác minh
Test: start/stop nhiều code với registry Prometheus thật, đếm series theo nhãn `stream_code` trước và sau, khẳng định không còn series của code đã dừng.
