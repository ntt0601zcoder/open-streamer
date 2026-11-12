# RTMP play không gửi lại sequence header khi nguồn đổi tham số codec
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** ingestor/push, publisher
- **Vị trí:** internal/ingestor/push/rtmp_writer.go:84-88, 113-117, 213-246, 255-264, 273-293, 315-325; internal/publisher/serve_rtmp.go:137, 147-155, 165-186

## Hiện tượng
Client RTMP đang xem một stream passthrough bị vỡ hình hoặc đứng khi nguồn đổi độ phân giải/profile, ví dụ failover sang backup 720p hoặc encoder kết nối lại với cấu hình mới. HLS/DASH vẫn tiếp tục bình thường. Client phải kết nối lại mới xem được.

## Điều kiện kích hoạt
1. Stream không transcode, viewer RTMP đang kết nối.
2. Input mới có SPS/PPS khác (hoặc sample rate AAC khác).

## Phân tích
Mỗi kết nối play có một writer, và trạng thái chỉ đi một chiều:
```go
avcSeqSent bool
aacSeqSent bool
...
if !w.avcSeqSent { ...; w.avcSeqSent = true }
```
`serve_rtmp.go` gặp `pkt.SessionStart` thì chỉ reset muxer:
```go
avMux = nil
tsCarry = nil
```
Không reset `cachedSeqSent`, `cachedSPS/PPS` hay writer. Trong khi đó, `buildAvccSliceOnly` loại bỏ SPS/PPS khỏi tag frame:
```go
case 7, 8, 9, 6: // SPS, PPS, AUD, SEI — exclude from NALU tag
    return
```
Do đó client nhận slice mới được mã hoá với SPS mới nhưng vẫn giải mã bằng cấu hình cũ, và không có cách nào nhận được SPS mới từ in-band. AAC tương tự với `aacSeqSent`, gây sai sample rate.

## Vì sao đây là lỗi
Comment trên `pkt.SessionStart` nói nguồn đã chuyển và cần state mới, nhưng state seq header bị bỏ sót. Trường hợp có transcoder không bị vì encoder giữ nguyên SPS, nên lỗi chỉ lộ ở stream passthrough, và test hiện không phủ tới.

## Hướng sửa
Cung cấp `RTMPFrameWriter.Reset` hoặc phát hiện SPS/PPS khác trong IDR rồi gửi lại AVC sequence header (và AAC ASC khi đổi). Gọi reset ở nhánh `SessionStart` và xoá `cachedSeqSent/cachedSPS/cachedPPS`.

## Cách xác minh
Unit test: `WriteFrame` với IDR có SPS A rồi IDR có SPS B, đếm số tag `AVCPacketType=0` (hiện là một). Thủ công: stream hai input có độ phân giải khác, mở `ffplay rtmp://...`, kích hoạt failover.
