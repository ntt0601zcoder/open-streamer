# POST /hooks/{id}/test báo delivered cả khi đích HTTP không tồn tại

- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** internal/hooks, internal/api/handler
- **Vị trí:** internal/hooks/service.go:157-165, internal/api/handler/hook.go:199-218

## Hiện tượng
Nút test trả `{"status":"delivered"}` kể cả khi URL sai hoặc server đích đang tắt. Operator tin hook đã hoạt động. Mã 502 trong swagger không bao giờ xuất hiện với hook HTTP.

## Điều kiện kích hoạt
Hook HTTP có target không truy cập được, gọi `POST /hooks/{id}/test`.

## Phân tích
```go
b := s.batcherFor(ctx, h)
b.enqueue(ev)
b.signal()
return nil
```
Handler coi `nil` là đã gửi:
```go
"status": "delivered",
```
Việc gửi thật diễn ra sau đó trong goroutine flusher, kết quả chỉ vào log và metric. Test event còn đi vào queue chung, nên khi đích hỏng nó bị thử lại cùng event thật.

## Vì sao đây là lỗi
Swagger khai báo `502 DELIVERY_FAILED` và mô tả "delivers a synthetic event"; handler có nhánh xử lý lỗi gửi nhưng với HTTP không có lỗi nào được trả về. Luồng file hook thì đồng bộ và báo lỗi đúng.

## Hướng sửa
Gửi test đồng bộ một lần bằng `postOnce` với body chỉ gồm event test, trả lỗi thật về handler, không dùng queue.

## Cách xác minh
Test với target là cổng đóng: `DeliverTestEvent` hiện trả `nil`.
