# Huỷ RTMP pull trong lúc đang dial làm rò rỉ phiên kết nối
- **Mức độ:** Thấp
- **Độ tin cậy:** Trung bình
- **Module liên quan:** ingestor/pull, thư viện lal
- **Vị trí:** internal/ingestor/pull/rtmp.go:114-116, 127-144, 175-183, 247-254; lal v0.37.4 pkg/rtmp/client_session.go:278-285, 308-317, 683-690

## Hiện tượng
Khi Stop, Probe hết hạn hoặc failover huỷ RTMP pull giữa lúc TCP/TLS còn đang kết nối, nếu kết nối sau đó thành công, sẽ có phiên RTMP mồ côi tiếp tục nhận từ upstream cho tới khi upstream tự đóng. Upstream thấy thêm một viewer ẩn, và server tốn băng thông, goroutine.

## Điều kiện kích hoạt
Upstream RTMP trả lời chậm, ctx bị huỷ trong khoảng từ `net.Dial`/`tls.Dial` bắt đầu tới khi trả về, và dial sau đó thành công.

## Phân tích
`Open` xử lý huỷ bằng `abortOpen` → `session.Dispose()`. Trong lal:
```go
s.disposeOnce.Do(func() {
    if s.conn == nil {
        retErr = base.ErrSessionNotStarted
        return
    }
    retErr = s.conn.Close()
})
```
`s.conn` chỉ được gán sau khi dial xong (dòng 317), nên Dispose trong lúc dial không đóng gì nhưng đã tiêu `disposeOnce`. Goroutine `connect` chạy tiếp handshake, play và `runReadLoop`. Không còn tham chiếu `session` (`r.session = nil`), Dispose lần sau cũng không có tác dụng. `ReadAvTimeoutMs = 0` nên không có giới hạn đọc.

Callback tiếp tục chạy với `r.converter`, là trường của reader. Khi reader mở lại và gán converter mới thì hai session cùng dùng một converter từ các goroutine khác nhau, là data race. `watchSessionEnd` chưa được khởi chạy cho phiên này.

## Vì sao đây là lỗi
Comment của `rtmp.go` nói "Open errors out cleanly without leaking goroutines on dial / handshake / play-start failures". Độ tin cậy Trung bình vì cửa sổ thời gian hẹp và chưa tái hiện được.

## Hướng sửa
Không dựa vào Dispose khi chưa có conn: chạy dial trong reader và truyền `net.Conn` đã kiểm soát ctx nếu lal cho phép, hoặc đợi `pullErr` rồi Dispose lại bằng session mới. Tối thiểu, callback bỏ qua khi session không còn là `r.session`.

## Cách xác minh
Test với TCP listener trì hoãn accept: gọi `Open` với ctx bị huỷ sớm, sau đó cho accept hoàn tất và phát phản hồi RTMP; kiểm tra phiên vẫn hoạt động và `goleak` phát hiện goroutine còn lại.
