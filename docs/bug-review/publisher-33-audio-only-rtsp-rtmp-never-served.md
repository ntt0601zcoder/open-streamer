# Stream chỉ có audio không bao giờ phát được qua RTSP và RTMP play
- **Mức độ:** Thấp
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTSP, RTMP play), ingestor/push
- **Vị trí:** internal/publisher/serve_rtsp.go:674-677, 651-668; internal/publisher/serve_rtmp.go:301-304; internal/ingestor/push/rtmp_writer.go:160-164

## Hiện tượng
Stream radio AAC-only: RTSP trả 404 mãi, RTMP play kết nối được nhưng không có dữ liệu, không có log lỗi.

## Điều kiện kích hoạt
Buffer phát chỉ có AAC.

## Phân tích
Cả hai đường giữ audio cho tới keyframe video đầu tiên, không có timeout:
```go
if sess.firstVideo { return } // hold audio until first video keyframe
```
RTSP chỉ `initStream` khi có SPS/PPS, và `initStream` luôn tạo media video. RTMP thêm một lần chặn ở `WriteFrame` (`!w.avcSeqSent`).

## Vì sao đây là lỗi
Cổng này để đồng bộ khi có video, nhưng không có nhánh cho trường hợp video không tồn tại. DASH đã có đường riêng (`cutAudioOnly`, pairing timeout).

## Hướng sửa
Thêm timeout: sau khoảng thời gian không có video, khởi tạo đường audio-only (SDP chỉ có audio, RTMP metadata không có video).

## Cách xác minh
Test RTSP pipeline chỉ cấp AAC: sau timeout phải có mount với một media audio.
