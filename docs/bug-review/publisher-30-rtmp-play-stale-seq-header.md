# RTMP play không gửi lại sequence header khi failover đổi SPS/PPS
- **Mức độ:** Trung bình
- **Độ tin cậy:** Cao
- **Module liên quan:** publisher (RTMP play), ingestor/push
- **Vị trí:** internal/publisher/serve_rtmp.go:147-155, 165-187; internal/ingestor/push/rtmp_writer.go:113-117, 213-246, 273-293, 315-325

## Hiện tượng
Client RTMP đang xem bị vỡ hình hoặc đứng hình sau failover sang nguồn khác độ phân giải hay tham số encoder, tới khi kết nối lại.

## Điều kiện kích hoạt
Stream passthrough (không transcode giữ encoder) có client RTMP play, rồi failover sang input có SPS/PPS khác. AAC đổi sample rate cũng gặp vấn đề tương tự.

## Phân tích
Pipeline chỉ reset muxer và carry ở `pkt.SessionStart`. `cachedSeqSent` và `avcSeqSent`/`aacSeqSent` latch cho cả phiên:
```go
if !w.avcSeqSent { ... }
```
Trong khi `buildAvccSliceOnly` loại SPS/PPS khỏi mọi tag video, nên client không thể nhận tham số mới theo bất kỳ đường nào. RTSP giữ NALU trong frame, còn RTMP push tự reconnect để phát lại header.

## Vì sao đây là lỗi
Comment ở `pushCodecAdapter` đã nêu cần phát lại khi SPS đổi; chỉ riêng play không có. Tag sau failover mang slice cần PPS/SPS mà decoder không có.

## Hướng sửa
So sánh SPS/PPS trên mỗi IDR, phát lại sequence header khi đổi. Tương tự với ASC của AAC, và cân nhắc `firstVideo=true` ở session boundary.

## Cách xác minh
Unit test `RTMPFrameWriter`: IDR có SPS mới phải phát seq header thứ hai trước tag NALU. Thực tế: failover giữa nguồn 1080p và 720p với ffplay RTMP.
