# RTSP làm sai timestamp của B-frame do kẹp đơn điệu trên PTS
- **Mức độ:** Cao
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTSP)
- **Vị trí:** internal/publisher/serve_rtsp.go:829, 867-887

## Hiện tượng
Video có B-frame phát qua RTSP bị giật, sai thứ tự hiển thị hoặc nhịp không đều. ffmpeg copy nhận PTS không hợp lý. Luồng không có B-frame thì bình thường.

## Điều kiện kích hoạt
Nguồn hoặc encoder có B-frame, thứ tự giải mã ví dụ PTS (ms): `0(I), 120(P), 40(B), 80(B), 240(P), ...`.

## Phân tích
`writeVideoRTP` truyền PTS vào `computeVideoRTP`, rồi hàm này ép timestamp tăng dần theo thứ tự giải mã:
```go
rel := pts - sess.baseDTS
rtpTS := uint32(rel * 90)
if rtpTS <= sess.lastVideoRTP {
    rtpTS = sess.lastVideoRTP + 1
}
```
Với dãy trên, RTP ra là `0, 120ms, 120ms+1tick, 120ms+2tick, 240ms`. Hai B-frame mất PTS thật (40 và 80 ms) và bị dồn sát P-frame.

## Vì sao đây là lỗi
RTP timestamp của H.264 là thời điểm trình chiếu (chính comment ở dòng 826 trích RFC 6184), nên nó phải được phép giảm theo thứ tự giải mã khi có B-frame. Kẹp đơn điệu chỉ đúng với DTS. Ngưỡng `rtspMaxBackJumpMs` chỉ quyết định rebase, không ngăn kẹp.

## Hướng sửa
Theo dõi tính đơn điệu trên DTS để phát hiện gián đoạn, giữ RTP timestamp = PTS - base mà không kẹp. Nếu cần chặn jitter thì chỉ áp dụng khi `pts == dts`.

## Cách xác minh
Unit test gọi `computeVideoRTP` với dãy decode-order trên và kiểm tra B-frame nhận 40 ms x 90 và 80 ms x 90. Thực tế: `ffprobe -show_frames` luồng RTSP từ nguồn x264 `bframes=2`.
