# Subscribe chạy song song với Delete tạo subscriber mồ côi, không bao giờ nhận EOF
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** buffer
- **Vị trí:** internal/buffer/service.go:82-88,106-113,242-250,269-278

## Hiện tượng
Hiếm khi một viewer hoặc copy tap đã subscribe thành công nhưng không nhận packet nào và cũng không thấy channel đóng. Nó treo cho đến khi context bị hủy hoặc packet timeout kích hoạt.

## Điều kiện kích hoạt
`Subscribe` đọc được `rb` và nhả `s.mu`. Đúng lúc đó `Delete` xóa map và chạy `unsubscribeAll`. Sau đó `rb.subscribe` thêm subscriber vào ring buffer đã chết. Xảy ra khi có client kết nối đúng lúc stream dừng hoặc reload.

## Phân tích
```go
s.mu.RLock(); rb, ok := s.buffers[id]; s.mu.RUnlock()
...
return rb.subscribe(s.cfg.Capacity), nil
```
`ringBuffer` không có cờ closed, nên `subscribe` sau `unsubscribeAll` vẫn thành công. Không ai ghi hay đóng channel đó nữa.

## Vì sao đây là lỗi
Docstring của `Delete` cam kết consumer thấy EOF sạch và mixer/copy dựa vào đó để retry. Cửa sổ race rất nhỏ nên mức độ thấp.

## Hướng sửa
Thêm `closed bool` trong `ringBuffer`, đặt trong `Delete` dưới `rb.mu`. `subscribe` trả lỗi hoặc trả channel đã đóng nếu `closed`. Lưu ý `UnsubscribeAll` để lại buffer sống nên không đặt cờ.

## Cách xác minh
Chèn hook hoặc chạy stress với `-race`: lặp Subscribe và Delete song song, kiểm tra mọi subscriber nhận được close trong thời hạn.
