# Cập nhật template không chạm tới runtime stream dù API hướng dẫn làm vậy

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api, internal/autopublish, internal/coordinator
- **Vị trí:** internal/api/handler/template.go:240-263, internal/api/handler/stream.go:376-385, internal/autopublish/service.go:194

## Hiện tượng
`PUT` trực tiếp vào runtime stream bị từ chối với thông báo "update the referenced template to change shared config". Operator sửa template (ladder, protocols, push) thì stream đang chạy không đổi, tới khi encoder rời đủ lâu và stream được tạo lại.

## Điều kiện kích hoạt
Có runtime stream đang chạy, `POST /templates/<code>` thay đổi cấu hình.

## Phân tích
`reloadStreams` chỉ duyệt store:
```go
streams, err := h.streamRepo.List(ctx, store.StreamFilter{})
```
Runtime stream "must never reach the store" nên không bao giờ được duyệt. Cấu hình của chúng là bản đã resolve khi tạo, không lưu cho lần cập nhật sau.

## Vì sao đây là lỗi
Comment và thông báo lỗi trong `StreamHandler.Put` xem template là đường cập nhật cho "every dependent runtime stream", và `TemplateHandler` có sẵn tham chiếu autopublish, nhưng chỉ dùng để làm mới matcher.

## Hướng sửa
Bổ sung duyệt `autopublish.ListRuntime()` theo `TemplateCode`, tính old/new resolve từ stub `{Code, Template}` và gọi `coordinator.Update`. Lưu ý các bug liên quan tới encoder đang push khi reload (flow-lifecycle-06).

## Cách xác minh
Test handler: runtime lister giả trả một entry cùng template, `Put` template và khẳng định `coordinator.Update` được gọi cho code đó.
