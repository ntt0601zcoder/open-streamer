# Cập nhật template không đến được stream auto-publish đang chạy

- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/autopublish, internal/coordinator, internal/publisher
- **Vị trí:** internal/api/handler/template.go:240-263, internal/api/handler/template.go:208-224, internal/api/handler/stream.go:371-375, internal/autopublish/service.go:194-210, internal/publisher/service.go:293-294

## Hiện tượng
Operator đổi ladder, protocols, `playback_policy` hoặc `stream_key` của template có prefix. Stream runtime đang phát vẫn dùng cấu hình cũ. Nếu template bị gỡ policy hoặc đổi policy để chặn truy cập, người xem stream runtime không bị ảnh hưởng. Encoder đẩy liên tục nên stream có thể giữ cấu hình cũ vô thời hạn.

## Điều kiện kích hoạt
1. Encoder đẩy vào một đường dẫn khớp prefix, tạo stream runtime.
2. `POST /templates/{code}` đổi `playback_policy` hoặc `transcoder`.

## Phân tích
`reloadStreams` chỉ duyệt store, mà stream runtime theo thiết kế không bao giờ nằm trong store:

```go
streams, err := h.streamRepo.List(ctx, store.StreamFilter{})
```

`TemplateHandler` có sẵn `autopublish *autopublish.Service` nhưng chỉ dùng để `RefreshTemplates` (matcher prefix). `ResolveOrCreate` chỉ resolve một lần khi tạo, và fast path `s.entries[code]` không xem lại template. Publisher giữ bản sao `playbackPolicy` và `streamKey` từ lần `Start` đó.

`TemplateHandler.Delete` cũng chỉ kiểm tra `findReferencingStreams` trên store, nên template đang có stream runtime dùng vẫn xoá được.

## Vì sao đây là lỗi
Thông báo 409 của `StreamHandler.Put` hướng dẫn "update the referenced template to change shared config", và comment ngay trên đó nói việc này áp dụng cho "every dependent runtime stream". Đường code đó không tồn tại.

## Hướng sửa
Trong `reloadStreams`, duyệt thêm `autopublish.ListRuntime()` với entry có `TemplateCode` tương ứng, dựng stub `{Code, Template}` rồi gọi `coordinator.Update` với bản resolve theo `prior` và `next`. Xử lý tương tự cho `Delete`.

## Cách xác minh
Test với fake autopublish có một entry: `Put` template phải gọi `coordinator.Update` cho code đó. Thử thực tế: đẩy RTMP vào prefix, gán policy yêu cầu token cho template, HLS không token vẫn trả 200.
