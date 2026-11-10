# Cập nhật template không áp dụng cho runtime stream đang chạy
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** api/handler, autopublish, coordinator
- **Vị trí:** internal/api/handler/template.go:240-263; internal/api/handler/stream.go:371-375; internal/autopublish/service.go:103-110, 194

## Hiện tượng
Operator sửa template để đổi ladder, protocol hoặc push đích. Các stream cấu hình kế thừa template được cập nhật, nhưng stream auto-publish đang live vẫn chạy cấu hình cũ cho tới khi encoder ngắt đủ lâu để stream bị reap.

## Điều kiện kích hoạt
Có encoder đang push vào một đường dẫn khớp prefix của template, sau đó `POST /templates/{code}` thay đổi cấu hình.

## Phân tích
`reloadStreams` chỉ duyệt store:
```go
streams, err := h.streamRepo.List(ctx, store.StreamFilter{})
```
Runtime stream không bao giờ nằm trong store. Sau đó `TemplateHandler.Put` chỉ gọi `RefreshTemplates`, và hàm này chỉ thay matcher prefix. `autopublish.Service` không có đường nào gọi `coordinator.Update` cho entry đang sống.

## Vì sao đây là lỗi
Khi từ chối `PUT` lên runtime stream, handler hướng dẫn operator: cập nhật template để đổi cấu hình dùng chung "for every dependent runtime stream". Đây là con đường duy nhất được chỉ dẫn, và nó không có tác dụng.

## Hướng sửa
Sau khi lưu template, duyệt `autopublish.ListRuntime()` theo `TemplateCode` và gọi `coordinator.Update` với view resolve theo template cũ và mới, giống stream cấu hình. Nếu chủ ý là không reload thì cần sửa thông báo lỗi và tài liệu.

## Cách xác minh
Test handler với autopublish stub có một runtime entry trỏ template. Sau `Put`, kiểm tra `coordinator.Update` được gọi cho code đó. Trên server: push RTMP vào prefix, tắt DASH trong template, kiểm tra DASH của stream đó.
