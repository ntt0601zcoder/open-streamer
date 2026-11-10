# `DELETE /sessions/{id}` không ngắt kết nối RTMP, SRT, RTSP, HTTP-TS
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher, sessions, api
- **Vị trí:** internal/publisher/sessions_helper.go:107, 123, 138, 162; internal/sessions/tracker.go:102-105, 397-432, 525-532; internal/api/handler/session.go:108-115

## Hiện tượng
Người vận hành kick một viewer, API trả 204 và session biến mất khỏi danh sách, phát event `kicked`, nhưng viewer vẫn xem tiếp bình thường và vẫn chiếm subscriber cùng slot trong `connLimiter`. Session không còn hiện trong API nên càng khó phát hiện.

## Điều kiện kích hoạt
Kick một session connection-bound (RTMP, SRT, RTSP, MPEGTS).

## Phân tích
`Kick` chỉ gọi `closeByID`, xóa bản ghi trong map và phát event. `Closer` chỉ có `Close`/`Touch`, không có đường báo về transport. Publisher bỏ luôn giá trị trả về:
```go
_, c := t.OpenConn(ctx, sessions.ConnHit{ ... })
```
Không có context hay callback nào gắn với kết nối để kick hủy.

## Vì sao đây là lỗi
Tracker mô tả Kick là "force-closes an active session", FEATURES_CHECKLIST ghi "Kick (force-close): Complete". USER_GUIDE có mục "Kick a viewer". Sau khi kick, các lần `Touch`/`Close` về sau là no-op với bản ghi đã xóa, nên hệ thống mất dấu viewer đang hoạt động.

## Hướng sửa
Thêm callback/cancel theo session: `ConnHit` nhận `Terminate func()` (đóng `conn`, gọi `Dispose`, hoặc hủy context request); `Kick` gọi nó. RTSP có thể gọi `ServerSession.Close`.

## Cách xác minh
Mở RTMP/SRT play, gọi DELETE session, kiểm tra kết nối đóng và số subscriber của buffer giảm.
