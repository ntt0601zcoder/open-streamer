# Client HTTP-TS ngừng đọc giữ goroutine, subscriber và slot giới hạn vô thời hạn
- **Mức độ:** Trung bình
- **Độ tin cậy:** Trung bình
- **Module liên quan:** publisher (HTTP-TS), api
- **Vị trí:** internal/publisher/serve_mpegts.go:85-90, 168-198; internal/api/server.go:423-426

## Hiện tượng
Các kết nối `/mpegts` treo không nhận dữ liệu vẫn nằm trong danh sách phát. Sau khi tích lũy tới giới hạn (256 mỗi stream), client mới nhận `503 too many connections`. Stream đã dừng nhưng goroutine vẫn còn.

## Điều kiện kích hoạt
Client mở `/mpegts`, sau đó không đọc socket nhưng vẫn ACK TCP (zero window), ví dụ tiến trình bị đóng băng hoặc client cố ý.

## Phân tích
`w.Write` nằm trực tiếp trong vòng select nên khi TCP send buffer đầy, handler chặn tại đó:
```go
n, err := w.Write(data)
```
`ctx.Done()` và `sub.Recv()` đóng không được kiểm tra trong lúc chặn. `http.Server` tạo với chỉ `Addr` và `Handler`, không có `WriteTimeout` (và không thể dùng timeout toàn cục cho stream dài), handler cũng không dùng `http.ResponseController.SetWriteDeadline`. `limiter.release` và `Unsubscribe` chỉ chạy khi handler thoát.

## Vì sao đây là lỗi
`connLimiter` được thêm để chống cạn tài nguyên do client không xác thực, nhưng một client không đọc có thể giữ slot mãi với chi phí rất thấp. TCP zero-window probe được ACK nên kernel không tự đóng.

## Hướng sửa
Trước mỗi lần ghi, gọi `ResponseController.SetWriteDeadline(now + N giây)`. Ghi lỗi hoặc quá hạn thì thoát handler.

## Cách xác minh
Client test mở kết nối, đặt receive buffer nhỏ và ngừng đọc; sau N giây subscriber phải giảm và slot phải được trả.
