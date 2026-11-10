# stream.started được phát trước stream.created, và create lỗi thì mất event

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/api/handler, internal/coordinator, internal/events, internal/hooks
- **Vị trí:** internal/api/handler/stream.go:423-426, internal/api/handler/stream.go:452-465, internal/coordinator/coordinator.go:413-416, internal/coordinator/coordinator.go:961-965

## Hiện tượng
Khi tạo stream, consumer nhận `stream.started` cho một stream mà nó chưa biết tới, rồi mới nhận `stream.created`. Nếu `Start` lỗi, API trả 500 nhưng bản ghi đã lưu và reconciler sẽ chạy nó sau tối đa 10 giây; consumer nhận `stream.started` mà không bao giờ có `stream.created`.

## Điều kiện kích hoạt
1. `POST /streams/new` với input hợp lệ, không `disabled`.
2. Trường hợp lỗi: lúc `Start` publisher hoặc transcoder lỗi tạm thời.

## Phân tích
Thứ tự trong `Put` là `Save`, rồi `coordinator.Start` (publish `EventStreamStarted` ở cuối `startLocked`), rồi mới publish `EventStreamCreated`. Nhánh lỗi return trước:

```go
if err := h.coordinator.Start(pipelineCtx, resolvedBody); err != nil {
    serverError(w, r, "START_FAILED", "start stream pipeline", err)
    return
}
```

Tương tự, `Update` lỗi sau khi đã `Save` thì không có `stream.updated`.

## Vì sao đây là lỗi
Bảng invariant ghi "Hooks must see consistent timeline". Bản ghi đã tồn tại kể từ `Save`, nên `created` phản ánh sự thật đó bất kể pipeline có khởi động được hay không.

## Hướng sửa
Publish `stream.created` ngay sau `Save` thành công, trước khi gọi coordinator. Bus có nhiều worker nên không bảo đảm tuyệt đối thứ tự, nhưng thứ tự vào hàng đợi sẽ đúng.

## Cách xác minh
Test handler với bus ghi lại thứ tự và coordinator giả trả lỗi: phải có `stream.created`.
