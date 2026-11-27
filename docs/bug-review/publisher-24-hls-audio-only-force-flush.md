# HLS audio-only trên đường AV: segment dài 4 lần và mỗi segment có DISCONTINUITY
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (HLS)
- **Vị trí:** internal/publisher/hls.go:548-551, 604-622

## Hiện tượng
Stream chỉ có tiếng (radio qua RTMP/RTSP) có segment `4 x segSec` (8 giây với mặc định), playlist có `#EXT-X-DISCONTINUITY` trước mọi segment, nghe tiếng lách cách ở ranh giới và độ trễ cao. Log `HLS force-flush without IDR` lặp mãi.

## Điều kiện kích hoạt
Buffer chỉ chứa AV AAC, không có video. Packet audio không bao giờ có `KeyFrame=true`.

## Phân tích
Đường AV chỉ cắt khi gặp keyframe:
```go
func (p *hlsSegmenter) maybeFlushAtKeyframe(...) { if !av.KeyFrame { return } ...
```
Nên audio-only chỉ cắt ở nhánh force-flush của `tickFlush`:
```go
if p.scanner == nil {
    p.discardUntilIDR = true
    p.discNext = true
```
`discNext` gắn DISCONTINUITY cho segment sau, lặp lại ở mọi chu kỳ. `discardUntilIDR` cũng bật mãi.

## Vì sao đây là lỗi
Force-flush được thiết kế cho "pathological long-GOP", còn audio-only không có IDR để chờ. DASH có nhánh riêng `cutAudioOnly`, cho thấy audio-only là trường hợp được hỗ trợ.

## Hướng sửa
Theo dõi đã thấy video chưa. Nếu không có video, cắt theo PTS audio khi đủ `segDur`, không đặt `discNext`/`discardUntilIDR`.

## Cách xác minh
Unit test: chỉ cấp AAC AV trong vài `segDur`, kiểm tra độ dài segment gần `segSec` và không có DISCONTINUITY ngoài lần đầu.
