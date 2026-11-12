# Một stream reload lỗi khiến matcher prefix không được cập nhật

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/autopublish, internal/coordinator, internal/events
- **Vị trí:** internal/api/handler/template.go:158-181, internal/api/handler/template.go:248-262, internal/autopublish/service.go:103-110

## Hiện tượng
`POST /templates/{code}` thêm prefix mới, một stream phụ thuộc reload lỗi và API trả 500. Template đã lưu, nhưng prefix mới không hoạt động và push vào đó bị từ chối. Prefix đã gỡ thì vẫn còn hoạt động. Không có event `template.updated`.

## Điều kiện kích hoạt
Template có stream phụ thuộc đang chạy, trong đó `coordinator.Update` lỗi (ví dụ `tc.Start` thất bại trong `reloadTranscoderFull`), và cùng request có đổi `prefixes`.

## Phân tích
Thứ tự là lưu, reload, rồi refresh:

```go
if err := h.reloadStreams(pipelineCtx, prior, &body); err != nil {
    serverError(w, r, "RELOAD_FAILED", "reload dependent streams", err)
    return
}

h.refreshAutoPublish(pipelineCtx)
```

`reloadStreams` đã gom lỗi để một stream hỏng không chặn các stream khác, nhưng lỗi tổng hợp đó lại chặn matcher và event. Matcher giữ snapshot cũ tới lần lưu template thành công tiếp theo.

## Vì sao đây là lỗi
Store và matcher trong bộ nhớ lệch nhau mặc dù template đã được ghi. Việc refresh matcher không phụ thuộc vào kết quả reload stream.

## Hướng sửa
Gọi `refreshAutoPublish` và phát event ngay sau `Save`, trước khi xử lý lỗi của `reloadStreams`. Vẫn trả lỗi reload cho client.

## Cách xác minh
Test handler với coordinator giả trả lỗi, kiểm tra `RefreshTemplates` vẫn được gọi.
