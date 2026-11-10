# DASH không chờ IDR sau session boundary, segment sau failover bắt đầu giữa GOP
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher/dash, ingestor
- **Vị trí:** internal/publisher/dash/packager.go:384-389, 605-616, 771-773; internal/publisher/dash/segmenter.go:218-229

## Hiện tượng
Sau failover hoặc reconnect với nguồn vào giữa GOP, segment đầu tiên của phiên mới chứa P/B không tham chiếu được. Player chặt báo lỗi decode, player khác vỡ hình đến IDR kế.

## Điều kiện kích hoạt
Stream DASH không transcode, session boundary chuyển sang nguồn có thể bắt đầu giữa GOP như RTSP, UDP, SRT.

## Phân tích
`onSessionBoundary` xóa queue nhưng không đặt lại `firstIDRSeen`:
```go
_ = p.queue.PopVideo(p.queue.VideoLen())
_ = p.queue.PopAudio(p.queue.AudioLen())
p.seg.Reset()
```
`handleH264` chỉ bỏ frame không phải key khi `!p.firstIDRSeen`, nên P/B đầu phiên được đưa vào queue. `findIDRCutPoint` chấp nhận queue có frame đầu không phải IDR.

## Vì sao đây là lỗi
Nhánh safety-net đã phải đặt `firstIDRSeen=false` vì cùng lý do. Session boundary tạo ra đúng tình huống đó nhưng không xử lý. MPD khai `startWithSAP="1"` nên đây là vi phạm được công bố.

## Hướng sửa
Đặt `p.firstIDRSeen = false` trong `onSessionBoundary`.

## Cách xác minh
Unit test: gửi IDR, SessionStart, P-frame; queue video phải rỗng cho đến IDR tiếp theo.
