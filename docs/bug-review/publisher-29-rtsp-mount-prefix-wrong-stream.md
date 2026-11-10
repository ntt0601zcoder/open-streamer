# RTSP định tuyến theo tiền tố nên có thể phát nhầm stream và lách playback policy
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTSP), mediaauth
- **Vị trí:** internal/publisher/serve_rtsp.go:225-255, 305-325

## Hiện tượng
Client mở `rtsp://host/a/b/c` có thể nhận media của stream `a/b`, ngẫu nhiên theo từng lần gọi. Khi `a/b/c` chưa mount, client vẫn nhận `a/b` thay vì 404.

## Điều kiện kích hoạt
Có mount `a/b` (hoặc `live/x`) và client yêu cầu path dài hơn cùng tiền tố, ví dụ `a/b/c` hoặc `live/x/y`.

## Phân tích
```go
for mountPath, stream := range mounts {
    if path == mountPath || strings.HasPrefix(path, mountPath+"/") {
        return stream
    }
}
```
Tiền tố được dùng để hỗ trợ `/trackID=N`, nhưng nó khớp mọi path con. Thứ tự duyệt map Go là ngẫu nhiên nên khi cả `a/b` và `a/b/c` đều mount, kết quả không xác định.

Kiểm tra quyền dùng code khác với stream được chọn: `OnPlay` gọi `playAllowed` với `rtspPathStreamCode(ctx.Path)`, tức code của path yêu cầu.

## Vì sao đây là lỗi
Client xin path `a/b/c` (không có policy, hoặc có token hợp lệ cho nó) có thể xem nội dung `a/b` có policy chặt hơn. `rtspPathStreamCode` đã biết cách bỏ `/trackID=`, nên không cần khớp tiền tố tùy ý.

## Hướng sửa
Chuẩn hóa path bằng cách bỏ đúng hậu tố `/trackID=N` rồi tra map chính xác. Dùng cùng code đã chuẩn hóa cho cả chọn stream và phân quyền.

## Cách xác minh
Unit test `matchRTSPMount` với `a/b` và path `a/b/c` phải trả nil; `a/b/trackID=0` vẫn trả `a/b`. Test OnDescribe với hai mount lồng nhau chạy lặp nhiều lần.
